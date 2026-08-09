package authweb

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dosfsociety/valorant-bot/internal/riot"
)

const (
	riotBrowserDiscoveryInterval = 25 * time.Millisecond
	riotBrowserInputCommitDelay  = 50 * time.Millisecond
	riotBrowserResponseLimit     = 1 << 20
	riotBrowserNavigationRetries = 8
)

type browserPasswordAuthClient interface {
	BrowserAuthorizeURL(state string) (string, error)
	AdoptBrowserLogin(ctx context.Context, responseBody []byte, cookies []*http.Cookie, userAgent string) (riot.PasswordTokens, *riot.MFAChallenge, error)
}

type riotBrowserLoginResult struct {
	ResponseBody []byte
	Cookies      []*http.Cookie
	UserAgent    string
}

type riotCaptchaSurface struct {
	X      float64
	Y      float64
	Width  float64
	Height float64
}

type riotCaptchaSurfaceSnapshot struct {
	Surface             riotCaptchaSurface
	DocumentToken       string
	SanitizerGeneration uint64
	MutationEpoch       uint64
	DevicePixelRatio    float64
	Integrity           bool
}

var errRiotCaptchaSurfaceUnavailable = errors.New("Riot CAPTCHA challenge surface is unavailable")

const riotCaptchaDocumentCurtainScript = `(function(){
if(window.top!==window)return;
const key=Symbol.for('valorant.remote-captcha-curtain');
const randomToken=()=>{const bytes=new Uint8Array(16);crypto.getRandomValues(bytes);return Array.from(bytes,b=>b.toString(16).padStart(2,'0')).join('')};
let state=window[key];
if(!state){
  const token=randomToken();
  state={armed:false,trustedSubmit:false,style:null,observer:null,rootAttribute:'data-remote-captcha-curtain-'+token,rootValue:randomToken()};
  Object.defineProperty(window,key,{value:state,configurable:false,writable:false});
}
const rootSelector='html['+state.rootAttribute+'="'+state.rootValue+'"]';
const css='html,body{background:#111722!important;color:transparent!important}html::before,html::after,body::before,body::after,'+rootSelector+'::before,'+rootSelector+'::after,'+rootSelector+' body::before,'+rootSelector+' body::after{content:none!important;display:none!important;visibility:hidden!important;opacity:0!important;pointer-events:none!important;background:none!important}html body *,html body *::before,html body *::after{visibility:hidden!important;opacity:0!important;pointer-events:none!important;color:transparent!important;-webkit-text-fill-color:transparent!important;text-shadow:none!important;background-color:transparent!important;background-image:none!important;border-color:transparent!important;outline:none!important;box-shadow:none!important;caret-color:transparent!important;content:none!important}';
const ensure=()=>{
  if(!document.documentElement)return;
  if(document.documentElement.getAttribute(state.rootAttribute)!==state.rootValue)document.documentElement.setAttribute(state.rootAttribute,state.rootValue);
  const root=document.head||document.documentElement;if(!root)return;
  let style=state.style;
  if(!style||!style.isConnected||style.textContent!==css){
    style=document.createElement('style');style.setAttribute('data-remote-captcha-curtain','');style.textContent=css;
    root.appendChild(style);state.style=style;
  }
  style.disabled=state.armed;
};
const block=event=>{if(!state.armed&&!state.trustedSubmit){event.preventDefault();event.stopImmediatePropagation()}};
for(const type of ['pointerdown','pointerup','pointermove','click','mousedown','mouseup','wheel','touchstart','touchend'])window.addEventListener(type,block,true);
if(!state.observer){state.observer=new MutationObserver(ensure);state.observer.observe(document,{subtree:true,childList:true,attributes:true})}
ensure();
})()`

const riotCaptchaSurfaceExpression = `(function(){
if(location.origin!=="https://authenticate.riotgames.com") return {originOK:false,ready:false};
const stateKey=Symbol.for('valorant.remote-captcha-sanitizer');
const curtain=window[Symbol.for('valorant.remote-captcha-curtain')];
const randomToken=()=>{const bytes=new Uint8Array(16);crypto.getRandomValues(bytes);return Array.from(bytes,b=>b.toString(16).padStart(2,'0')).join('')};
let state=window[stateKey];
if(!state){
  const token=randomToken();
  state={documentToken:randomToken(),generation:1,mutationEpoch:0,attribute:'data-remote-captcha-surface-'+token,value:randomToken(),rootAttribute:'data-remote-captcha-root-'+token,rootValue:randomToken(),styleID:'remote-captcha-sanitizer-'+token,frame:null,src:'',rect:null,observers:new Map(),integrity:false};
  Object.defineProperty(window,stateKey,{value:state,configurable:false,writable:false});
}
state.integrity=false;
if(!document.documentElement||!document.body)return {originOK:true,ready:false,documentToken:state.documentToken,sanitizerGeneration:state.generation,devicePixelRatio:devicePixelRatio,integrity:false};
if(document.documentElement.getAttribute(state.rootAttribute)!==state.rootValue)document.documentElement.setAttribute(state.rootAttribute,state.rootValue);
const rootSelector='html['+state.rootAttribute+'="'+state.rootValue+'"]';
const css='html,body{background:#111722!important;color:transparent!important}html::before,html::after,body::before,body::after,'+rootSelector+'::before,'+rootSelector+'::after,'+rootSelector+' body::before,'+rootSelector+' body::after{content:none!important;display:none!important;visibility:hidden!important;opacity:0!important;pointer-events:none!important;background:none!important}body *,body *::before,body *::after{visibility:hidden!important;opacity:0!important;pointer-events:none!important;color:transparent!important;-webkit-text-fill-color:transparent!important;text-shadow:none!important;background-color:transparent!important;background-image:none!important;border-color:transparent!important;outline:none!important;box-shadow:none!important;caret-color:transparent!important;content:none!important}body iframe['+state.attribute+'="'+state.value+'"]{visibility:visible!important;opacity:1!important;pointer-events:auto!important}';
let style=document.getElementById(state.styleID);
if(!style||style.tagName!=='STYLE'||style.textContent!==css){
  state.generation++;
  if(style)style.remove();
  style=document.createElement('style');style.id=state.styleID;style.textContent=css;(document.head||document.documentElement).appendChild(style);
}
const disarm=()=>{if(curtain){curtain.armed=false;if(curtain.style)curtain.style.disabled=false}};
const roots=[document],allElements=[];
for(let index=0;index<roots.length;index++){
  const root=roots[index];
  for(const element of root.querySelectorAll('*')){
    allElements.push(element);
    if(element.shadowRoot)roots.push(element.shadowRoot);
  }
}
if(!state.observers)state.observers=new Map();
const activeRoots=new Set(roots);
for(const [root,observer] of state.observers){if(!activeRoots.has(root)){observer.disconnect();state.observers.delete(root);state.generation++;state.integrity=false;disarm()}}
for(const root of roots){
  if(state.observers.has(root))continue;
	  const observer=new MutationObserver(()=>{state.mutationEpoch++});
  observer.observe(root===document?document.documentElement:root,{subtree:true,childList:true,attributes:true,characterData:true});
  state.observers.set(root,observer);state.generation++;
}
const visibleRect=element=>{
	const computed=getComputedStyle(element); if(computed.display==='none')return null;
  const rect=element.getBoundingClientRect();
  const left=Math.max(0,rect.left),top=Math.max(0,rect.top),right=Math.min(innerWidth,rect.right),bottom=Math.min(innerHeight,rect.bottom);
  if(right-left<32||bottom-top<32)return null;
  return {x:left,y:top,width:right-left,height:bottom-top,area:(right-left)*(bottom-top)};
};
const hcaptchaURL=value=>{try{const host=new URL(value,location.href).hostname.toLowerCase();return host==='hcaptcha.com'||host.endsWith('.hcaptcha.com')}catch(_){return false}};
const candidates=[];
for(const root of roots){for(const frame of root.querySelectorAll('iframe')){if(hcaptchaURL(frame.src))candidates.push(frame)}}
let best=null;
let selected=null;
for(const element of candidates){const rect=visibleRect(element);if(rect&&(!best||rect.area>best.area)){best=rect;selected=element}}
for(const root of roots){for(const marked of root.querySelectorAll('iframe['+state.attribute+']')){if(marked!==selected)marked.removeAttribute(state.attribute)}}
if(!best){if(state.frame){state.generation++;state.frame=null;state.src='';state.rect=null}state.integrity=false;disarm();for(const observer of state.observers.values())observer.takeRecords();return {originOK:true,ready:false,documentToken:state.documentToken,sanitizerGeneration:state.generation,mutationEpoch:state.mutationEpoch,devicePixelRatio:devicePixelRatio,integrity:false}}
const nextRect=[best.x,best.y,best.width,best.height];
if(state.frame!==selected||state.src!==selected.src||!state.rect||state.rect.some((value,index)=>Math.abs(value-nextRect[index])>.25)){state.generation++}
state.frame=selected;state.src=selected.src;state.rect=nextRect;
selected.setAttribute(state.attribute,state.value);
const keep=new Set();let node=selected;while(node){keep.add(node);const root=node.getRootNode&&node.getRootNode();node=node.parentElement||(root&&root.host)||null}
for(const element of allElements){
  const exact=element===selected,ancestor=keep.has(element);
  if(exact){element.style.setProperty('visibility','visible','important');element.style.setProperty('opacity','1','important');element.style.setProperty('pointer-events','auto','important');continue}
  element.style.setProperty('pointer-events','none','important');element.style.setProperty('color','transparent','important');element.style.setProperty('-webkit-text-fill-color','transparent','important');element.style.setProperty('text-shadow','none','important');element.style.setProperty('background-color','transparent','important');element.style.setProperty('background-image','none','important');element.style.setProperty('border-color','transparent','important');element.style.setProperty('box-shadow','none','important');element.style.setProperty('caret-color','transparent','important');
  if(ancestor){element.style.setProperty('visibility','visible','important');element.style.setProperty('opacity','1','important')}
  else{element.style.setProperty('visibility','hidden','important');element.style.setProperty('opacity','0','important')}
  if(element instanceof HTMLInputElement||element instanceof HTMLTextAreaElement){element.value='';element.removeAttribute('value');element.removeAttribute('placeholder');element.removeAttribute('aria-label')}
  if(element instanceof HTMLSelectElement){element.selectedIndex=-1;element.removeAttribute('value');element.removeAttribute('aria-label')}
}
for(const rootSurface of [document.documentElement,document.body]){if(rootSurface){rootSurface.style.setProperty('background-color','#111722','important');rootSurface.style.setProperty('background-image','none','important')}}
for(const observer of state.observers.values())observer.takeRecords();
const marked=[];for(const root of roots){for(const frame of root.querySelectorAll('iframe['+state.attribute+'="'+state.value+'"]'))marked.push(frame)}
const computed=getComputedStyle(selected);
const pseudoHidden=computed=>{const content=(computed.content||'').trim();const ungenerated=content==='none'||content==='normal';const visuallySuppressed=computed.display==='none'||computed.visibility==='hidden'||Number(computed.opacity)===0;return (ungenerated||visuallySuppressed)&&computed.pointerEvents==='none'};
const pseudoIntegrity=pseudoHidden(getComputedStyle(document.documentElement,'::before'))&&pseudoHidden(getComputedStyle(document.documentElement,'::after'))&&pseudoHidden(getComputedStyle(document.body,'::before'))&&pseudoHidden(getComputedStyle(document.body,'::after'));
const integrity=style.isConnected&&style.textContent===css&&document.documentElement.getAttribute(state.rootAttribute)===state.rootValue&&marked.length===1&&marked[0]===selected&&hcaptchaURL(selected.src)&&computed.visibility==='visible'&&Number(computed.opacity)===1&&computed.pointerEvents==='auto'&&pseudoIntegrity;
state.integrity=integrity;
if(curtain){curtain.armed=integrity;if(curtain.style)curtain.style.disabled=integrity}
return {originOK:true,ready:integrity,x:best.x,y:best.y,width:best.width,height:best.height,documentToken:state.documentToken,sanitizerGeneration:state.generation,mutationEpoch:state.mutationEpoch,devicePixelRatio:devicePixelRatio,integrity};
})()`

type riotBrowserLoginController interface {
	captchaBrowserController
	RunRiotLogin(ctx context.Context, username, password string) (riotBrowserLoginResult, error)
}

func (c *chromeBrowserController) riotCaptchaReadyChannel() <-chan struct{} {
	c.riotCaptchaMu.Lock()
	defer c.riotCaptchaMu.Unlock()
	if c.riotCaptchaReady == nil {
		c.riotCaptchaReady = make(chan struct{})
	}
	return c.riotCaptchaReady
}

func (c *chromeBrowserController) publishRiotCaptchaSurface(surface riotCaptchaSurface, err error) {
	if c == nil {
		return
	}
	c.riotCaptchaMu.Lock()
	if c.riotCaptchaReady == nil {
		c.riotCaptchaReady = make(chan struct{})
	}
	if c.riotCaptchaPublished {
		c.riotCaptchaMu.Unlock()
		return
	}
	c.riotCaptchaPublished = true
	c.riotCaptchaSurface = surface
	c.riotCaptchaSurfaceErr = err
	ready := c.riotCaptchaReady
	c.riotCaptchaMu.Unlock()
	c.riotCaptchaReadyOnce.Do(func() { close(ready) })
}

func (c *chromeBrowserController) waitRiotCaptchaSurface(ctx context.Context) (riotCaptchaSurface, error) {
	if c == nil {
		return riotCaptchaSurface{}, errRiotCaptchaSurfaceUnavailable
	}
	ready := c.riotCaptchaReadyChannel()
	if hook := c.beforeRiotCaptchaReadyWaitForTest; hook != nil {
		hook()
	}
	select {
	case <-ready:
	case <-ctx.Done():
		return riotCaptchaSurface{}, ctx.Err()
	}
	c.riotCaptchaMu.Lock()
	defer c.riotCaptchaMu.Unlock()
	if c.riotCaptchaSurfaceErr != nil {
		return riotCaptchaSurface{}, c.riotCaptchaSurfaceErr
	}
	if !validRiotCaptchaSurface(c.riotCaptchaSurface) {
		return riotCaptchaSurface{}, errRiotCaptchaSurfaceUnavailable
	}
	return c.riotCaptchaSurface, nil
}

func (c *chromeBrowserController) RunRiotLogin(ctx context.Context, username, password string) (riotBrowserLoginResult, error) {
	if c == nil || strings.TrimSpace(c.profileDir) == "" {
		return riotBrowserLoginResult{}, errors.New("captcha Chrome profile is unavailable")
	}
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return riotBrowserLoginResult{}, riot.ErrPasswordInvalid
	}
	if c.devToolsPipe == nil {
		return riotBrowserLoginResult{}, errors.New("official Riot login requires a private Chrome DevTools pipe")
	}
	client, err := c.chromeDevToolsClient()
	if err != nil {
		return riotBrowserLoginResult{}, err
	}
	if err := client.attachRiotPage(ctx); err != nil {
		return riotBrowserLoginResult{}, err
	}
	for _, method := range []string{"Network.enable", "Runtime.enable", "Page.enable"} {
		if err := client.Call(ctx, method, map[string]any{}, nil); err != nil {
			return riotBrowserLoginResult{}, err
		}
	}
	if err := c.ensureRiotCaptchaDocumentCurtain(ctx, client); err != nil {
		return riotBrowserLoginResult{}, err
	}
	networkEvents, err := client.SubscribeEvents(client.currentSessionID(),
		"Network.requestWillBeSent", "Network.responseReceived", "Network.loadingFinished")
	if err != nil {
		return riotBrowserLoginResult{}, fmt.Errorf("watch Riot browser login: %w", err)
	}
	defer networkEvents.Close()
	if err := client.submitRiotCredentials(ctx, username, password); err != nil {
		c.publishRiotCaptchaSurface(riotCaptchaSurface{}, err)
		return riotBrowserLoginResult{}, err
	}
	result, err := client.waitForRiotLogin(ctx, networkEvents, func(surface riotCaptchaSurface) {
		c.publishRiotCaptchaSurface(surface, nil)
	})
	if err != nil {
		c.publishRiotCaptchaSurface(riotCaptchaSurface{}, err)
	} else {
		c.publishRiotCaptchaSurface(riotCaptchaSurface{}, errors.New("Riot login completed before a CAPTCHA challenge surface was available"))
	}
	return result, err
}

func (c *chromeDevToolsClient) installRiotCaptchaDocumentCurtain(ctx context.Context) error {
	var installed struct {
		Identifier string `json:"identifier"`
	}
	if err := c.Call(ctx, "Page.addScriptToEvaluateOnNewDocument", map[string]any{
		"source": riotCaptchaDocumentCurtainScript, "runImmediately": true,
	}, &installed); err != nil {
		return fmt.Errorf("install remote CAPTCHA document curtain: %w", err)
	}
	if strings.TrimSpace(installed.Identifier) == "" {
		return errors.New("install remote CAPTCHA document curtain: empty script identifier")
	}
	return nil
}

func (c *chromeBrowserController) ensureRiotCaptchaDocumentCurtain(ctx context.Context, client *chromeDevToolsClient) error {
	if c == nil || client == nil {
		return errChromeDevToolsClientClosed
	}
	sessionID := strings.TrimSpace(client.currentSessionID())
	if sessionID == "" {
		return errors.New("install remote CAPTCHA document curtain: Chrome session is unavailable")
	}
	c.riotCaptchaCurtainMu.Lock()
	defer c.riotCaptchaCurtainMu.Unlock()
	if c.riotCaptchaCurtainSession == sessionID {
		return nil
	}
	if err := client.installRiotCaptchaDocumentCurtain(ctx); err != nil {
		return err
	}
	c.riotCaptchaCurtainSession = sessionID
	return nil
}

func (c *chromeDevToolsClient) attachRiotPage(ctx context.Context) error {
	ticker := time.NewTicker(riotBrowserDiscoveryInterval)
	defer ticker.Stop()
	for {
		var targets struct {
			TargetInfos []struct {
				TargetID string `json:"targetId"`
				Type     string `json:"type"`
				URL      string `json:"url"`
			} `json:"targetInfos"`
		}
		if err := c.Call(ctx, "Target.getTargets", map[string]any{}, &targets); err != nil {
			return fmt.Errorf("discover Riot Chrome page: %w", err)
		}
		for _, target := range targets.TargetInfos {
			if target.Type != "page" || target.TargetID == "" || !allowedRiotBrowserPage(target.URL) {
				continue
			}
			var attached struct {
				SessionID string `json:"sessionId"`
			}
			if err := c.Call(ctx, "Target.attachToTarget", map[string]any{
				"targetId": target.TargetID,
				"flatten":  true,
			}, &attached); err != nil {
				return fmt.Errorf("attach Riot Chrome page: %w", err)
			}
			if strings.TrimSpace(attached.SessionID) == "" {
				return errors.New("attach Riot Chrome page: empty session")
			}
			c.setSessionID(attached.SessionID)
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("discover Riot Chrome page: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func allowedRiotBrowserPage(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") {
		return false
	}
	// Chrome is launched at auth.riotgames.com/authorize, which redirects the
	// same page target to the credential form. Do not attach while it is still
	// on the authorize document: credential evaluation is intentionally scoped
	// to the exact authenticate.riotgames.com origin.
	return strings.EqualFold(parsed.Host, RiotCaptchaHost)
}

func validRiotCaptchaSurface(surface riotCaptchaSurface) bool {
	values := [...]float64{surface.X, surface.Y, surface.Width, surface.Height}
	for _, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return false
		}
	}
	return surface.X >= 0 && surface.Y >= 0 && surface.Width >= 32 && surface.Height >= 32 &&
		surface.Width <= remoteCaptchaViewportWidth && surface.Height <= remoteCaptchaViewportHeight &&
		surface.X+surface.Width <= remoteCaptchaViewportWidth && surface.Y+surface.Height <= remoteCaptchaViewportHeight
}

func (c *chromeDevToolsClient) riotCaptchaSurface(ctx context.Context) (riotCaptchaSurface, error) {
	snapshot, err := c.evaluateRiotCaptchaSurface(ctx, false)
	return snapshot.Surface, err
}

func (c *chromeDevToolsClient) riotCaptchaSurfaceSnapshot(ctx context.Context) (riotCaptchaSurfaceSnapshot, error) {
	return c.evaluateRiotCaptchaSurface(ctx, true)
}

func (c *chromeDevToolsClient) guardRiotCaptchaInput(ctx context.Context, snapshot riotCaptchaSurfaceSnapshot, x, y float64) error {
	documentJSON, _ := json.Marshal(snapshot.DocumentToken)
	expression := `(function(){
if(location.origin!=="https://authenticate.riotgames.com")return {originOK:false,ok:false};
const state=window[Symbol.for('valorant.remote-captcha-sanitizer')];if(!state)return {originOK:true,ok:false};
const rect=state.frame&&state.frame.getBoundingClientRect();
const pseudoHidden=computed=>{const content=(computed.content||'').trim();const ungenerated=content==='none'||content==='normal';const visuallySuppressed=computed.display==='none'||computed.visibility==='hidden'||Number(computed.opacity)===0;return (ungenerated||visuallySuppressed)&&computed.pointerEvents==='none'};
const pseudoIntegrity=document.body&&pseudoHidden(getComputedStyle(document.documentElement,'::before'))&&pseudoHidden(getComputedStyle(document.documentElement,'::after'))&&pseudoHidden(getComputedStyle(document.body,'::before'))&&pseudoHidden(getComputedStyle(document.body,'::after'));
const same=state.integrity===true&&pseudoIntegrity&&state.documentToken===` + string(documentJSON) + `&&state.generation===` + fmt.Sprint(snapshot.SanitizerGeneration) + `&&rect&&
Math.abs(rect.left-` + fmt.Sprint(snapshot.Surface.X) + `)<.01&&Math.abs(rect.top-` + fmt.Sprint(snapshot.Surface.Y) + `)<.01&&
Math.abs(rect.width-` + fmt.Sprint(snapshot.Surface.Width) + `)<.01&&Math.abs(rect.height-` + fmt.Sprint(snapshot.Surface.Height) + `)<.01;
return {originOK:true,ok:!!same&&document.elementFromPoint(` + fmt.Sprint(x) + `,` + fmt.Sprint(y) + `)===state.frame};
})()`
	var evaluated struct {
		Result struct {
			Value struct {
				OriginOK bool `json:"originOK"`
				OK       bool `json:"ok"`
			} `json:"value"`
		} `json:"result"`
		ExceptionDetails json.RawMessage `json:"exceptionDetails"`
	}
	if err := c.Call(ctx, "Runtime.evaluate", map[string]any{"expression": expression, "returnByValue": true, "awaitPromise": true}, &evaluated); err != nil {
		return fmt.Errorf("guard remote CAPTCHA input: %w", err)
	}
	if len(evaluated.ExceptionDetails) != 0 || !evaluated.Result.Value.OriginOK || !evaluated.Result.Value.OK {
		return errRemoteCaptchaInputInvalid
	}
	return nil
}

func (c *chromeDevToolsClient) evaluateRiotCaptchaSurface(ctx context.Context, requireIntegrity bool) (riotCaptchaSurfaceSnapshot, error) {
	var evaluated struct {
		Result struct {
			Value struct {
				OriginOK            bool    `json:"originOK"`
				Ready               bool    `json:"ready"`
				X                   float64 `json:"x"`
				Y                   float64 `json:"y"`
				Width               float64 `json:"width"`
				Height              float64 `json:"height"`
				DocumentToken       string  `json:"documentToken"`
				SanitizerGeneration uint64  `json:"sanitizerGeneration"`
				MutationEpoch       uint64  `json:"mutationEpoch"`
				DevicePixelRatio    float64 `json:"devicePixelRatio"`
				Integrity           bool    `json:"integrity"`
			} `json:"value"`
		} `json:"result"`
		ExceptionDetails json.RawMessage `json:"exceptionDetails"`
	}
	if err := c.Call(ctx, "Runtime.evaluate", map[string]any{
		"expression":    riotCaptchaSurfaceExpression,
		"returnByValue": true,
		"awaitPromise":  true,
	}, &evaluated); err != nil {
		return riotCaptchaSurfaceSnapshot{}, fmt.Errorf("inspect Riot CAPTCHA challenge: %w", err)
	}
	if len(evaluated.ExceptionDetails) != 0 {
		return riotCaptchaSurfaceSnapshot{}, errors.New("inspect Riot CAPTCHA challenge: evaluation failed")
	}
	if !evaluated.Result.Value.OriginOK {
		return riotCaptchaSurfaceSnapshot{}, fmt.Errorf("inspect Riot CAPTCHA challenge: %w: exact authentication origin changed", errRiotCaptchaDocumentChanged)
	}
	if !evaluated.Result.Value.Ready {
		return riotCaptchaSurfaceSnapshot{}, errRiotCaptchaSurfaceUnavailable
	}
	surface := riotCaptchaSurface{
		X: evaluated.Result.Value.X, Y: evaluated.Result.Value.Y,
		Width: evaluated.Result.Value.Width, Height: evaluated.Result.Value.Height,
	}
	if !validRiotCaptchaSurface(surface) {
		return riotCaptchaSurfaceSnapshot{}, errors.New("inspect Riot CAPTCHA challenge: invalid surface bounds")
	}
	snapshot := riotCaptchaSurfaceSnapshot{
		Surface: surface, DocumentToken: evaluated.Result.Value.DocumentToken,
		SanitizerGeneration: evaluated.Result.Value.SanitizerGeneration,
		MutationEpoch:       evaluated.Result.Value.MutationEpoch,
		DevicePixelRatio:    evaluated.Result.Value.DevicePixelRatio, Integrity: evaluated.Result.Value.Integrity,
	}
	if requireIntegrity && (strings.TrimSpace(snapshot.DocumentToken) == "" || snapshot.SanitizerGeneration == 0 ||
		!finiteRemoteCaptchaNumber(snapshot.DevicePixelRatio) || snapshot.DevicePixelRatio <= 0 || !snapshot.Integrity) {
		return riotCaptchaSurfaceSnapshot{}, errRiotCaptchaSurfaceUnavailable
	}
	return snapshot, nil
}

func (c *chromeDevToolsClient) waitForRiotCaptchaSurface(ctx context.Context) (riotCaptchaSurface, error) {
	navigationRetries := 0
	for {
		surface, err := c.riotCaptchaSurface(ctx)
		if err == nil {
			return surface, nil
		}
		if !errors.Is(err, errRiotCaptchaSurfaceUnavailable) {
			if !retryRiotBrowserNavigationError(err, &navigationRetries) {
				return riotCaptchaSurface{}, err
			}
		}
		if waitErr := waitRiotBrowserDiscovery(ctx); waitErr != nil {
			return riotCaptchaSurface{}, waitErr
		}
	}
}

func (c *chromeDevToolsClient) submitRiotCredentials(ctx context.Context, username, password string) error {
	usernameJSON, _ := json.Marshal(username)
	passwordJSON, _ := json.Marshal(password)
	fillExpression := `(function(){
if(location.origin!=="https://authenticate.riotgames.com") return {originOK:false,filled:false};
if(document.readyState !== 'complete') return {originOK:true,filled:false};
const roots=[document]; for(let i=0;i<roots.length;i++){for(const el of roots[i].querySelectorAll('*')){if(el.shadowRoot) roots.push(el.shadowRoot)}}
const find=(selectors)=>{for(const root of roots){for(const selector of selectors){const el=root.querySelector(selector); if(el && !el.disabled) return el}} return null};
const username=find(['input[name="username"]','input[autocomplete="username"]','input[data-testid*="username"]','input[type="text"]']);
const password=find(['input[name="password"]','input[autocomplete="current-password"]','input[data-testid*="password"]','input[type="password"]']);
if(!username || !password) return {originOK:true,filled:false};
const set=(el,value)=>{const setter=Object.getOwnPropertyDescriptor(HTMLInputElement.prototype,'value').set; setter.call(el,value); el.dispatchEvent(new Event('input',{bubbles:true})); el.dispatchEvent(new Event('change',{bubbles:true}))};
set(username,` + string(usernameJSON) + `); set(password,` + string(passwordJSON) + `);
return {originOK:true,filled:true};
})()`
	navigationRetries := 0
	for {
		var evaluated struct {
			Result struct {
				Value struct {
					OriginOK bool `json:"originOK"`
					Filled   bool `json:"filled"`
				} `json:"value"`
			} `json:"result"`
			ExceptionDetails json.RawMessage `json:"exceptionDetails"`
		}
		err := c.Call(ctx, "Runtime.evaluate", map[string]any{
			"expression":    fillExpression,
			"returnByValue": true,
			"awaitPromise":  true,
		}, &evaluated)
		if err != nil {
			if !retryRiotBrowserNavigationError(err, &navigationRetries) {
				return fmt.Errorf("fill Riot browser login: %w", err)
			}
			if waitErr := waitRiotBrowserDiscovery(ctx); waitErr != nil {
				return fmt.Errorf("fill Riot browser login: %w", waitErr)
			}
			continue
		}
		if len(evaluated.ExceptionDetails) != 0 {
			return errors.New("fill Riot browser login: credential injection failed")
		}
		if !evaluated.Result.Value.OriginOK {
			return errors.New("fill Riot browser login: exact authentication origin changed")
		}
		if evaluated.Result.Value.Filled {
			break
		}
		if waitErr := waitRiotBrowserDiscovery(ctx); waitErr != nil {
			return fmt.Errorf("fill Riot browser login: %w", waitErr)
		}
	}

	commitTimer := time.NewTimer(riotBrowserInputCommitDelay)
	defer commitTimer.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("submit Riot browser login: %w", ctx.Err())
	case <-commitTimer.C:
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("submit Riot browser login: %w", err)
		}
	}

	submitExpression := `(function(){
if(location.origin!=="https://authenticate.riotgames.com") return {originOK:false,submitted:false};
if(document.readyState !== 'complete') return {originOK:true,submitted:false};
const curtain=window[Symbol.for('valorant.remote-captcha-curtain')];
if(!curtain)return {originOK:true,submitted:false};
const roots=[document]; for(let i=0;i<roots.length;i++){for(const el of roots[i].querySelectorAll('*')){if(el.shadowRoot) roots.push(el.shadowRoot)}}
let captchaInitialized=false;
for(const root of roots){if(root.querySelector('.h-captcha,[data-hcaptcha-widget-id],iframe[src*="hcaptcha.com"]')){captchaInitialized=true;break}}
if(!captchaInitialized){
  const now=performance.now();
  if(!Number.isFinite(curtain.submitWaitStarted))curtain.submitWaitStarted=now;
  if(now-curtain.submitWaitStarted<2000)return {originOK:true,submitted:false};
}
for(const root of roots){
  const password=root.querySelector('input[name="password"],input[autocomplete="current-password"],input[data-testid*="password"],input[type="password"]');
	  const form=password && password.form; const button=form ? form.querySelector('button[data-testid="btn-signin-submit"]') : null;
	  if(button && !button.disabled){
	    curtain.submitWaitStarted=undefined;
	    curtain.trustedSubmit=true;
	    try{button.click();return {originOK:true,submitted:true}}finally{curtain.trustedSubmit=false}
	  }
}
return {originOK:true,submitted:false};
})()`
	navigationRetries = 0
	for {
		var evaluated struct {
			Result struct {
				Value struct {
					OriginOK  bool `json:"originOK"`
					Submitted bool `json:"submitted"`
				} `json:"value"`
			} `json:"result"`
			ExceptionDetails json.RawMessage `json:"exceptionDetails"`
		}
		err := c.Call(ctx, "Runtime.evaluate", map[string]any{
			"expression":    submitExpression,
			"returnByValue": true,
			"awaitPromise":  true,
		}, &evaluated)
		if err != nil {
			if !retryRiotBrowserNavigationError(err, &navigationRetries) {
				return fmt.Errorf("submit Riot browser login: %w", err)
			}
			if waitErr := waitRiotBrowserDiscovery(ctx); waitErr != nil {
				return fmt.Errorf("submit Riot browser login: %w", waitErr)
			}
			continue
		}
		if len(evaluated.ExceptionDetails) != 0 {
			return errors.New("submit Riot browser login: submit click failed")
		}
		if !evaluated.Result.Value.OriginOK {
			return errors.New("submit Riot browser login: exact authentication origin changed")
		}
		if evaluated.Result.Value.Submitted {
			return nil
		}
		if waitErr := waitRiotBrowserDiscovery(ctx); waitErr != nil {
			return fmt.Errorf("submit Riot browser login: %w", waitErr)
		}
	}
}

func waitRiotBrowserDiscovery(ctx context.Context) error {
	timer := time.NewTimer(riotBrowserDiscoveryInterval)
	defer timer.Stop()
	return waitRiotBrowserDiscoveryEvent(ctx, timer.C, nil, nil)
}

// waitRiotBrowserDiscoveryEvent keeps the cancellation decision testable
// without changing the production timer. Hooks are nil outside tests.
func waitRiotBrowserDiscoveryEvent(ctx context.Context, timer <-chan time.Time, beforeSelect, afterTimer func()) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if beforeSelect != nil {
		beforeSelect()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer:
		if afterTimer != nil {
			afterTimer()
		}
		// Prefer cancellation even when it becomes ready with the timer.
		if err := ctx.Err(); err != nil {
			return err
		}
		return nil
	}
}

func retryRiotBrowserNavigationError(err error, retries *int) bool {
	var protocolErr *chromeDevToolsProtocolError
	if !errors.As(err, &protocolErr) || protocolErr.Method != "Runtime.evaluate" || retries == nil {
		return false
	}
	message := strings.ToLower(protocolErr.Message)
	navigationAbort := strings.Contains(message, "execution context was destroyed") ||
		strings.Contains(message, "cannot find default execution context") ||
		strings.Contains(message, "inspected target navigated or closed") ||
		strings.Contains(message, "not attached to an active page")
	if !navigationAbort || *retries >= riotBrowserNavigationRetries {
		return false
	}
	*retries++
	return true
}

type riotBrowserRequest struct {
	method   string
	rawURL   string
	response bool
	status   int
}

func (c *chromeDevToolsClient) waitForRiotLogin(ctx context.Context, events *chromeDevToolsEventSubscription, publishCaptcha func(riotCaptchaSurface)) (riotBrowserLoginResult, error) {
	requests := make(map[string]riotBrowserRequest)
	for {
		event, err := events.Next(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return riotBrowserLoginResult{}, ctx.Err()
			}
			return riotBrowserLoginResult{}, fmt.Errorf("watch Riot browser login: %w", err)
		}
		switch event.Method {
		case "Network.requestWillBeSent":
			var params struct {
				RequestID string `json:"requestId"`
				Request   struct {
					URL    string `json:"url"`
					Method string `json:"method"`
				} `json:"request"`
			}
			if json.Unmarshal(event.Params, &params) == nil {
				requests[params.RequestID] = riotBrowserRequest{method: params.Request.Method, rawURL: params.Request.URL}
				if endpoint, hasQuery := riotBrowserLoginEndpoint(params.Request.URL); endpoint {
					log.Printf("Riot browser login request started method=%s query=%t", params.Request.Method, hasQuery)
				}
			}
		case "Network.responseReceived":
			var params struct {
				RequestID string `json:"requestId"`
				Response  struct {
					URL    string `json:"url"`
					Status int    `json:"status"`
				} `json:"response"`
			}
			if json.Unmarshal(event.Params, &params) == nil {
				request := requests[params.RequestID]
				request.response = true
				request.status = params.Response.Status
				if request.rawURL == "" {
					request.rawURL = params.Response.URL
				}
				requests[params.RequestID] = request
			}
		case "Network.loadingFinished":
			var params struct {
				RequestID string `json:"requestId"`
			}
			if json.Unmarshal(event.Params, &params) != nil {
				continue
			}
			request := requests[params.RequestID]
			delete(requests, params.RequestID)
			endpoint, _ := riotBrowserLoginEndpoint(request.rawURL)
			if request.response && endpoint && request.status == http.StatusOK && request.method != http.MethodPut {
				body, bodyErr := c.riotResponseBody(ctx, params.RequestID)
				if bodyErr != nil {
					log.Printf("Riot browser login discovery response unavailable: %v", bodyErr)
				} else {
					responseType, hasCaptcha := riotBrowserResponseSummary(body)
					log.Printf("Riot browser login discovery response method=%s type=%q captcha=%t", request.method, responseType, hasCaptcha)
					if hasCaptcha {
						surface, surfaceErr := c.waitForRiotCaptchaSurface(ctx)
						if surfaceErr != nil {
							return riotBrowserLoginResult{}, surfaceErr
						}
						if publishCaptcha != nil {
							publishCaptcha(surface)
						}
					}
				}
				continue
			}
			if request.method != http.MethodPut || !request.response || !isRiotBrowserLoginURL(request.rawURL) {
				continue
			}
			if request.status != http.StatusOK {
				log.Printf("Riot browser login response rejected status=%d", request.status)
				continue
			}
			body, err := c.riotResponseBody(ctx, params.RequestID)
			if err != nil {
				return riotBrowserLoginResult{}, err
			}
			if !riotBrowserLoginTerminal(body) {
				log.Printf("Riot browser CAPTCHA challenge response received")
				surface, surfaceErr := c.waitForRiotCaptchaSurface(ctx)
				if surfaceErr != nil {
					return riotBrowserLoginResult{}, surfaceErr
				}
				if publishCaptcha != nil {
					publishCaptcha(surface)
				}
				continue
			}
			cookies, err := c.riotBrowserCookies(ctx)
			if err != nil {
				return riotBrowserLoginResult{}, err
			}
			userAgent, err := c.riotBrowserUserAgent(ctx)
			if err != nil {
				return riotBrowserLoginResult{}, err
			}
			return riotBrowserLoginResult{ResponseBody: body, Cookies: cookies, UserAgent: userAgent}, nil
		}
	}
}

func isRiotBrowserLoginURL(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	return err == nil && strings.EqualFold(parsed.Scheme, "https") &&
		strings.EqualFold(parsed.Host, RiotCaptchaHost) && parsed.Path == "/api/v1/login" &&
		parsed.RawQuery == "" && parsed.Fragment == ""
}

func riotBrowserLoginEndpoint(rawURL string) (bool, bool) {
	parsed, err := url.Parse(rawURL)
	return err == nil && strings.EqualFold(parsed.Scheme, "https") &&
			strings.EqualFold(parsed.Host, RiotCaptchaHost) && parsed.Path == "/api/v1/login",
		parsed != nil && parsed.RawQuery != ""
}

func riotBrowserLoginTerminal(body []byte) bool {
	if len(body) == 0 || len(body) > riotBrowserResponseLimit {
		return false
	}
	var response struct {
		Type        string          `json:"type"`
		Error       string          `json:"error"`
		Success     json.RawMessage `json:"success"`
		Multifactor json.RawMessage `json:"multifactor"`
	}
	if json.Unmarshal(body, &response) != nil {
		return false
	}
	hasCaptcha := bytes.Contains(bytes.ToLower(body), []byte(`"hcaptcha"`))
	if response.Type == "success" || response.Type == "multifactor" || len(response.Multifactor) > 2 || len(response.Success) > 2 {
		return true
	}
	return response.Error != "" && !hasCaptcha
}

func riotBrowserResponseSummary(body []byte) (string, bool) {
	var response struct {
		Type    string          `json:"type"`
		Captcha json.RawMessage `json:"captcha"`
	}
	if json.Unmarshal(body, &response) != nil {
		return "", false
	}
	return response.Type, len(response.Captcha) > 2 || bytes.Contains(bytes.ToLower(body), []byte(`"hcaptcha"`))
}

func (c *chromeDevToolsClient) riotResponseBody(ctx context.Context, requestID string) ([]byte, error) {
	var response struct {
		Body          string `json:"body"`
		Base64Encoded bool   `json:"base64Encoded"`
	}
	if err := c.Call(ctx, "Network.getResponseBody", map[string]any{"requestId": requestID}, &response); err != nil {
		return nil, err
	}
	body := []byte(response.Body)
	if response.Base64Encoded {
		decoded, err := base64.StdEncoding.DecodeString(response.Body)
		if err != nil {
			return nil, fmt.Errorf("decode Riot browser response: %w", err)
		}
		body = decoded
	}
	if len(body) > riotBrowserResponseLimit {
		return nil, errors.New("Riot browser response exceeds limit")
	}
	return body, nil
}

func (c *chromeDevToolsClient) riotBrowserUserAgent(ctx context.Context) (string, error) {
	var evaluated struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	if err := c.Call(ctx, "Runtime.evaluate", map[string]any{
		"expression":    "navigator.userAgent",
		"returnByValue": true,
	}, &evaluated); err != nil {
		return "", err
	}
	userAgent := strings.TrimSpace(evaluated.Result.Value)
	if userAgent == "" {
		return "", errors.New("Riot browser user-agent is empty")
	}
	return userAgent, nil
}

func (c *chromeDevToolsClient) riotBrowserCookies(ctx context.Context) ([]*http.Cookie, error) {
	var response struct {
		Cookies []struct {
			Name     string  `json:"name"`
			Value    string  `json:"value"`
			Domain   string  `json:"domain"`
			Path     string  `json:"path"`
			Expires  float64 `json:"expires"`
			HTTPOnly bool    `json:"httpOnly"`
			Secure   bool    `json:"secure"`
			SameSite string  `json:"sameSite"`
		} `json:"cookies"`
	}
	if err := c.Call(ctx, "Network.getCookies", map[string]any{
		"urls": []string{"https://authenticate.riotgames.com/api/v1/login"},
	}, &response); err != nil {
		return nil, err
	}
	cookies := make([]*http.Cookie, 0, len(response.Cookies))
	for _, item := range response.Cookies {
		cookie := &http.Cookie{
			Name:     item.Name,
			Value:    item.Value,
			Domain:   item.Domain,
			Path:     item.Path,
			Secure:   item.Secure,
			HttpOnly: item.HTTPOnly,
		}
		if item.Expires > 0 {
			cookie.Expires = time.Unix(int64(item.Expires), 0)
		}
		switch strings.ToLower(item.SameSite) {
		case "strict":
			cookie.SameSite = http.SameSiteStrictMode
		case "lax":
			cookie.SameSite = http.SameSiteLaxMode
		case "none":
			cookie.SameSite = http.SameSiteNoneMode
		}
		cookies = append(cookies, cookie)
	}
	return cookies, nil
}

func (s *Server) runRiotBrowserLogin(ctx context.Context, state string, pending passwordPending, generation uint64, auth browserPasswordAuthClient, controller riotBrowserLoginController) {
	defer pending.flow.wg.Done()
	result, runErr := controller.RunRiotLogin(ctx, pending.username, pending.password)
	var (
		tokens    riot.PasswordTokens
		challenge *riot.MFAChallenge
		err       error
	)
	if runErr != nil {
		err = runErr
	} else {
		// Adoption may exchange a login token over the network. It must remain
		// outside launchMu so a reopen can take ownership and cancel this
		// generation instead of waiting behind an old upstream request.
		tokens, challenge, err = auth.AdoptBrowserLogin(ctx, result.ResponseBody, result.Cookies, result.UserAgent)
	}

	// Reopen and terminal publication are serialized by launchMu. A worker may
	// finish after its controller was closed; it must never seal the shared flow
	// or close the replacement browser.
	pending.flow.launchMu.Lock()
	defer pending.flow.launchMu.Unlock()
	if pending.flow.browserGeneration != generation || pending.flow.browser != controller {
		return
	}
	current, live := s.livePasswordState(state, "")
	if !live || current.flow != pending.flow {
		return
	}
	if err := s.claimPasswordFinalization(state, pending.flow); err != nil {
		return
	}
	closedController, closeErr := closeCaptchaBrowserLocked(pending.flow)
	s.recordCaptchaBrowserCloseResultLocked(pending.flow, closedController, closeErr, false)
	if err != nil {
		_, _ = s.publishFinalizedPasswordOutcome(state, pending.flow, passwordOutcome{err: err}, closeErr)
		return
	}
	if challenge != nil {
		mfaState, stateErr := newState()
		if stateErr != nil {
			_, _ = s.publishFinalizedPasswordOutcome(state, pending.flow, passwordOutcome{err: stateErr}, closeErr)
			return
		}
		_, _ = s.finishFinalizedPasswordMFA(state, current, mfaState, challenge, formatMFAHint(challenge), closeErr)
		return
	}
	_, _ = s.finishFinalizedPasswordAccount(pending.flow.ctx, state, current, tokens, closeErr)
}
