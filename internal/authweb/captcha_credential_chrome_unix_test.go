//go:build unix

package authweb

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// This test keeps DNS disabled and fulfills both documents through CDP Fetch,
// so it exercises Chromium rendering at Riot's exact origin without contacting
// Riot or hCaptcha.
func TestRemoteCaptchaLocalChromiumRendersOnlyCanonicalChallengeClip(t *testing.T) {
	testRemoteCaptchaLocalChromiumProductionPath(t, 2)
}

func testRemoteCaptchaLocalChromiumProductionPath(t *testing.T, forcedDeviceScaleFactor float64) {
	t.Helper()
	chrome := findChromeBinary()
	if strings.TrimSpace(chrome) == "" {
		t.Skip("Chrome/Chromium is not installed")
	}
	profile := t.TempDir()
	userDataDir := filepath.Join(profile, "chrome")
	cmd := exec.Command(chrome,
		"--headless=new", "--disable-gpu", "--no-sandbox", "--disable-background-networking",
		"--disable-component-update", "--disable-default-apps", "--disable-extensions", "--disable-sync",
		"--host-resolver-rules=MAP * ~NOTFOUND", "--force-device-scale-factor="+fmt.Sprint(forcedDeviceScaleFactor),
		"--remote-debugging-pipe", "--user-data-dir="+userDataDir, "about:blank",
	)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	setup, err := prepareChromeDevToolsPipe(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		setup.closeAll()
		t.Skipf("local Chrome could not start: %v", err)
	}
	setup.closeChildEnds()
	t.Cleanup(func() {
		_ = setup.host.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_, _ = cmd.Process.Wait()
		_ = os.RemoveAll(profile)
	})

	controller := &chromeBrowserController{profileDir: profile, devToolsPipe: setup.host}
	client, err := controller.chromeDevToolsClient()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var targets struct {
		TargetInfos []struct {
			TargetID string `json:"targetId"`
			Type     string `json:"type"`
		} `json:"targetInfos"`
	}
	if err := client.Call(ctx, "Target.getTargets", map[string]any{}, &targets); err != nil {
		t.Fatal(err)
	}
	var targetID string
	for _, target := range targets.TargetInfos {
		if target.Type == "page" {
			targetID = target.TargetID
			break
		}
	}
	if targetID == "" {
		t.Fatal("local Chrome did not expose a page target")
	}
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	if err := client.Call(ctx, "Target.attachToTarget", map[string]any{"targetId": targetID, "flatten": true}, &attached); err != nil {
		t.Fatal(err)
	}
	client.setSessionID(attached.SessionID)
	for _, method := range []string{"Page.enable", "Runtime.enable", "Fetch.enable"} {
		params := map[string]any{}
		if method == "Fetch.enable" {
			params["patterns"] = []map[string]any{{"urlPattern": "https://authenticate.riotgames.com/*"}, {"urlPattern": "https://hcaptcha.com/*"}}
		}
		if err := client.Call(ctx, method, params, nil); err != nil {
			t.Fatal(err)
		}
	}
	events, err := client.SubscribeEvents(attached.SessionID, "Fetch.requestPaused")
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	iframeFulfilled := make(chan struct{})
	var iframeFulfilledOnce sync.Once
	fulfillErr := make(chan error, 1)
	go func() {
		for {
			event, eventErr := events.Next(ctx)
			if eventErr != nil {
				fulfillErr <- eventErr
				return
			}
			var paused struct {
				RequestID string `json:"requestId"`
				Request   struct {
					URL string `json:"url"`
				} `json:"request"`
			}
			if json.Unmarshal(event.Params, &paused) != nil {
				continue
			}
			body := `<html id="app"><head><style id="credential-pseudos">html#app::before,body#login::after{content:""!important;display:block!important;visibility:visible!important;opacity:1!important;pointer-events:none!important;position:fixed!important;inset:0!important;background:#f00000!important;color:#fff!important;z-index:2147483647!important}</style></head><body id="login" style="margin:0;background:#f00"><input value="browser-user"><input value="raw-browser-password"><div id="mount"></div></body></html>`
			isIframe := strings.HasPrefix(paused.Request.URL, "https://hcaptcha.com/")
			if isIframe {
				body = `<html><body style="margin:0;background:transparent"><div style="position:absolute;inset:4px;background:#08c642"></div></body></html>`
			}
			callErr := client.Call(ctx, "Fetch.fulfillRequest", map[string]any{
				"requestId": paused.RequestID, "responseCode": 200,
				"responseHeaders": []map[string]any{{"name": "Content-Type", "value": "text/html; charset=utf-8"}},
				"body":            base64.StdEncoding.EncodeToString([]byte(body)),
			}, nil)
			if callErr != nil {
				fulfillErr <- callErr
				return
			}
			if isIframe {
				iframeFulfilledOnce.Do(func() { close(iframeFulfilled) })
			}
		}
	}()
	if err := client.Call(ctx, "Page.navigate", map[string]any{"url": "https://authenticate.riotgames.com/"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := client.riotCaptchaSurfaceSnapshot(ctx); !errors.Is(err, errRiotCaptchaSurfaceUnavailable) {
		t.Fatalf("pre-challenge surface error=%v, want unavailable", err)
	}
	waitEntered := make(chan struct{})
	controller.beforeRiotCaptchaReadyWaitForTest = func() { close(waitEntered) }
	flowCtx, flowCancel := context.WithCancel(ctx)
	processCtx, processCancel := context.WithCancel(ctx)
	viewerCtx, viewerCancel := context.WithCancel(ctx)
	serverCtx, serverCancel := context.WithCancel(ctx)
	defer flowCancel()
	defer processCancel()
	defer serverCancel()
	type startResult struct {
		stream *remoteCaptchaStream
		err    error
	}
	started := make(chan startResult, 1)
	go func() {
		stream, startErr := controller.StartRemoteCaptchaStream(flowCtx, processCtx, viewerCtx, serverCtx)
		started <- startResult{stream: stream, err: startErr}
	}()
	select {
	case <-waitEntered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	client.mu.Lock()
	commandsAtReadyWait := client.nextID
	client.mu.Unlock()
	if commandsAtReadyWait == 0 {
		t.Fatal("production readiness wait was not reached after browser setup")
	}
	var inserted struct{}
	if err := client.Call(ctx, "Runtime.evaluate", map[string]any{
		"expression":   `(function(){const host=document.createElement('div');document.getElementById('mount').appendChild(host);const root=host.attachShadow({mode:'open'});const f=document.createElement('iframe');f.src='https://hcaptcha.com/widget';f.style.cssText='position:absolute;left:20px;top:15px;width:80px;height:60px;border:0';root.appendChild(f)})()`,
		"awaitPromise": true, "returnByValue": true,
	}, &inserted); err != nil {
		t.Fatal(err)
	}
	select {
	case <-iframeFulfilled:
	case err := <-fulfillErr:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := client.Call(ctx, "Runtime.evaluate", map[string]any{
		"expression": "new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve)))", "awaitPromise": true,
	}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := client.riotCaptchaSurfaceSnapshot(ctx); !errors.Is(err, errRiotCaptchaSurfaceUnavailable) {
		t.Fatalf("high-specificity credential pseudo surface error=%v, want fail-closed unavailable", err)
	}
	select {
	case result := <-started:
		if result.stream != nil {
			_ = result.stream.Close(context.Background())
		}
		t.Fatalf("hostile pseudo challenge started production stream: %v", result.err)
	default:
	}
	if err := client.Call(ctx, "Runtime.evaluate", map[string]any{
		"expression":   `(function(){const sheet=document.getElementById('credential-pseudos').sheet;while(sheet.cssRules.length)sheet.deleteRule(0)})()`,
		"awaitPromise": true, "returnByValue": true,
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := client.Call(ctx, "Runtime.evaluate", map[string]any{
		"expression": "new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve)))", "awaitPromise": true,
	}, nil); err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.riotCaptchaSurfaceSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Call(ctx, "Runtime.evaluate", map[string]any{
		"expression": `document.getElementById('credential-pseudos').sheet.insertRule('html#app::before,body#login::after{content:""!important;display:block!important;visibility:visible!important;opacity:1!important;pointer-events:none!important;position:fixed!important;inset:0!important;background:#f00000!important;z-index:2147483647!important}')`,
	}, nil); err != nil {
		t.Fatal(err)
	}
	guardX := snapshot.Surface.X + snapshot.Surface.Width/2
	guardY := snapshot.Surface.Y + snapshot.Surface.Height/2
	if err := client.guardRiotCaptchaInput(ctx, snapshot, guardX, guardY); !errors.Is(err, errRemoteCaptchaInputInvalid) {
		t.Fatalf("empty-content credential pseudo input guard error=%v, want fail-closed rejection", err)
	}
	if err := client.Call(ctx, "Runtime.evaluate", map[string]any{
		"expression": `document.getElementById('credential-pseudos').sheet.deleteRule(0)`,
	}, nil); err != nil {
		t.Fatal(err)
	}
	snapshot, err = client.riotCaptchaSurfaceSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Call(ctx, "Runtime.evaluate", map[string]any{
		"expression": `(function(){const churn=document.createElement('div');churn.id='unrelated-churn';document.body.appendChild(churn);window.__remoteCaptchaTestChurn=setInterval(()=>churn.setAttribute('data-tick',String(performance.now())),1);setTimeout(()=>clearInterval(window.__remoteCaptchaTestChurn),120)})()`,
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := client.Call(ctx, "Runtime.evaluate", map[string]any{
		"expression": "new Promise(resolve=>setTimeout(resolve,40))", "awaitPromise": true,
	}, nil); err != nil {
		t.Fatal(err)
	}
	churnSnapshot, err := client.riotCaptchaSurfaceSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if churnSnapshot.Surface != snapshot.Surface || churnSnapshot.DocumentToken != snapshot.DocumentToken ||
		churnSnapshot.SanitizerGeneration != snapshot.SanitizerGeneration {
		t.Fatalf("unrelated DOM churn changed CAPTCHA identity before=%+v after=%+v", snapshot, churnSnapshot)
	}
	if churnSnapshot.MutationEpoch <= snapshot.MutationEpoch {
		t.Fatalf("unrelated DOM churn mutation epoch before=%d after=%d", snapshot.MutationEpoch, churnSnapshot.MutationEpoch)
	}
	viewport, err := (&remoteCaptchaStream{client: client}).remoteCaptchaLayoutMetrics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(viewport.Zoom-1) > .05 || math.Abs(snapshot.DevicePixelRatio-forcedDeviceScaleFactor) > .05 {
		t.Fatalf("installed Chrome metrics zoom=%v DPR=%v, want physical DPR %v without browser zoom", viewport.Zoom, snapshot.DevicePixelRatio, forcedDeviceScaleFactor)
	}
	controller.publishRiotCaptchaSurface(snapshot.Surface, nil)
	var startedStream startResult
	select {
	case startedStream = <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if startedStream.err != nil {
		t.Fatal(startedStream.err)
	}
	stream := startedStream.stream
	var curtain struct {
		Result struct {
			Value bool `json:"value"`
		} `json:"result"`
	}
	if err := client.Call(ctx, "Runtime.evaluate", map[string]any{
		"expression": "!!window[Symbol.for('valorant.remote-captcha-curtain')]", "returnByValue": true,
	}, &curtain); err != nil {
		t.Fatal(err)
	}
	if !curtain.Result.Value {
		t.Fatal("production StartRemoteCaptchaStream did not install the document-start curtain")
	}
	var frame remoteCaptchaOutputFrame
	select {
	case frame = <-stream.Frames():
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := client.Call(ctx, "Runtime.evaluate", map[string]any{
		"expression": "clearInterval(window.__remoteCaptchaTestChurn);delete window.__remoteCaptchaTestChurn",
	}, nil); err != nil {
		t.Fatal(err)
	}
	if frame.Binding.FrameWidth != 76 || frame.Binding.FrameHeight != 56 ||
		frame.Binding.FrameWidth > remoteCaptchaViewportWidth || frame.Binding.FrameHeight > remoteCaptchaViewportHeight {
		t.Fatalf("production frame=%dx%d, want exact bounded 76x56 CSS challenge clip", frame.Binding.FrameWidth, frame.Binding.FrameHeight)
	}
	if bytes.Contains(frame.JPEG, []byte("browser-user")) || bytes.Contains(frame.JPEG, []byte("raw-browser-password")) {
		t.Fatal("canonical Chromium frame retained credential bytes")
	}
	decoded, err := jpeg.Decode(bytes.NewReader(frame.JPEG))
	if err != nil {
		t.Fatal(err)
	}
	safe := [][3]int{{8, 198, 66}, {17, 23, 34}}
	for y := decoded.Bounds().Min.Y; y < decoded.Bounds().Max.Y; y++ {
		for x := decoded.Bounds().Min.X; x < decoded.Bounds().Max.X; x++ {
			r16, g16, b16, _ := decoded.At(x, y).RGBA()
			r, g, b := int(r16>>8), int(g16>>8), int(b16>>8)
			allowed := false
			for _, palette := range safe {
				dr, dg, db := r-palette[0], g-palette[1], b-palette[2]
				if dr*dr+dg*dg+db*db <= 120*120 {
					allowed = true
					break
				}
			}
			if !allowed || (r > g+60 && r > b+60) {
				t.Fatalf("Chromium output pixel (%d,%d) rgb=(%d,%d,%d) is outside the safe challenge palette", x, y, r, g, b)
			}
		}
	}
	viewerCancel()
	select {
	case <-stream.Done():
	case <-ctx.Done():
		t.Fatal("production stream ignored viewer lifetime cancellation")
	}
}
