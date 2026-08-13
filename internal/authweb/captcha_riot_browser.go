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
	"strconv"
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
  const left=Math.max(0,Math.round(rect.left)),top=Math.max(0,Math.round(rect.top)),right=Math.min(innerWidth,Math.round(rect.right)),bottom=Math.min(innerHeight,Math.round(rect.bottom));
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
	networkEvents, err := client.SubscribeEvents(client.currentSessionID(),
		"Network.requestWillBeSent", "Network.responseReceived", "Network.loadingFinished")
	if err != nil {
		return riotBrowserLoginResult{}, fmt.Errorf("watch Riot browser login: %w", err)
	}
	defer networkEvents.Close()
	for _, method := range []string{"Network.enable", "Runtime.enable", "Page.enable"} {
		if err := client.Call(ctx, method, map[string]any{}, nil); err != nil {
			return riotBrowserLoginResult{}, err
		}
	}
	if err := c.ensureRiotCaptchaDocumentCurtain(ctx, client); err != nil {
		return riotBrowserLoginResult{}, err
	}
	if err := client.fillRiotCredentials(ctx, username, password); err != nil {
		c.publishRiotCaptchaSurface(riotCaptchaSurface{}, err)
		return riotBrowserLoginResult{}, err
	}
	requiresCaptcha, err := client.prepareRiotBrowserLogin(ctx, networkEvents)
	if err != nil {
		c.publishRiotCaptchaSurface(riotCaptchaSurface{}, err)
		return riotBrowserLoginResult{}, err
	}
	var result riotBrowserLoginResult
	if requiresCaptcha {
		result, err = client.waitForRiotLoginAndCaptchaSurface(ctx, networkEvents, func(surface riotCaptchaSurface) {
			c.publishRiotCaptchaSurface(surface, nil)
		})
	} else {
		result, err = client.waitForRiotLogin(ctx, networkEvents, func(surface riotCaptchaSurface) {
			c.publishRiotCaptchaSurface(surface, nil)
		})
	}
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
Math.abs(rect.left-` + fmt.Sprint(snapshot.Surface.X) + `)<1&&Math.abs(rect.top-` + fmt.Sprint(snapshot.Surface.Y) + `)<1&&
Math.abs(rect.width-` + fmt.Sprint(snapshot.Surface.Width) + `)<1&&Math.abs(rect.height-` + fmt.Sprint(snapshot.Surface.Height) + `)<1;
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

func (c *chromeDevToolsClient) fillRiotCredentials(ctx context.Context, username, password string) error {
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

	return nil
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

type riotBrowserDocumentIdentity struct {
	frameID  string
	loaderID string
}

type riotBrowserSubmitTarget struct {
	document   riotBrowserDocumentIdentity
	token      string
	generation uint64
	buttonID   string
	widgetID   string
	legalID    string
	apiID      string
	captcha    bool
}

type riotBrowserDiscovery struct {
	captcha bool
}

func (c *chromeDevToolsClient) riotBrowserDocumentIdentity(ctx context.Context) (riotBrowserDocumentIdentity, error) {
	var tree struct {
		FrameTree struct {
			Frame struct {
				ID       string `json:"id"`
				LoaderID string `json:"loaderId"`
				URL      string `json:"url"`
			} `json:"frame"`
		} `json:"frameTree"`
	}
	if err := c.Call(ctx, "Page.getFrameTree", map[string]any{}, &tree); err != nil {
		return riotBrowserDocumentIdentity{}, fmt.Errorf("inspect Riot login document identity: %w", err)
	}
	parsed, err := url.Parse(tree.FrameTree.Frame.URL)
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || !strings.EqualFold(parsed.Host, RiotCaptchaHost) ||
		strings.TrimSpace(tree.FrameTree.Frame.ID) == "" || strings.TrimSpace(tree.FrameTree.Frame.LoaderID) == "" {
		return riotBrowserDocumentIdentity{}, errors.New("inspect Riot login document identity: exact authentication origin changed")
	}
	return riotBrowserDocumentIdentity{frameID: tree.FrameTree.Frame.ID, loaderID: tree.FrameTree.Frame.LoaderID}, nil
}

func (c *chromeDevToolsClient) prepareRiotBrowserLogin(ctx context.Context, events *chromeDevToolsEventSubscription) (bool, error) {
	discovery, err := c.waitForInitialRiotBrowserDiscovery(ctx, events)
	if err != nil {
		return false, err
	}
	target, err := c.waitForStableRiotBrowserSubmitTarget(ctx, discovery.captcha)
	if err != nil {
		return false, err
	}
	if err := c.clickRiotBrowserSubmitTarget(ctx, target); err != nil {
		return false, err
	}
	return discovery.captcha, nil
}

type riotBrowserLoginWatchResult struct {
	result riotBrowserLoginResult
	err    error
}

type riotBrowserSurfaceWatchResult struct {
	surface riotCaptchaSurface
	err     error
}

func (c *chromeDevToolsClient) waitForRiotLoginAndCaptchaSurface(ctx context.Context, events *chromeDevToolsEventSubscription, publishCaptcha func(riotCaptchaSurface)) (riotBrowserLoginResult, error) {
	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	loginResults := make(chan riotBrowserLoginWatchResult, 1)
	surfaceResults := make(chan riotBrowserSurfaceWatchResult, 1)
	go func() {
		result, err := c.waitForRiotLoginState(watchCtx, events, nil, false)
		loginResults <- riotBrowserLoginWatchResult{result: result, err: err}
	}()
	go func() {
		surface, err := c.waitForRiotCaptchaSurface(watchCtx)
		surfaceResults <- riotBrowserSurfaceWatchResult{surface: surface, err: err}
	}()

	for surfaceResults != nil {
		select {
		case login := <-loginResults:
			return login.result, login.err
		case surface := <-surfaceResults:
			surfaceResults = nil
			if surface.err == nil && publishCaptcha != nil {
				publishCaptcha(surface.surface)
			}
		case <-ctx.Done():
			return riotBrowserLoginResult{}, ctx.Err()
		}
	}
	select {
	case login := <-loginResults:
		return login.result, login.err
	case <-ctx.Done():
		return riotBrowserLoginResult{}, ctx.Err()
	}
}

func (c *chromeDevToolsClient) waitForInitialRiotBrowserDiscovery(ctx context.Context, events *chromeDevToolsEventSubscription) (riotBrowserDiscovery, error) {
	if events == nil {
		return riotBrowserDiscovery{}, errors.New("watch Riot browser discovery: event subscription is unavailable")
	}
	requests := make(map[string]riotBrowserRequest)
	for {
		event, err := events.Next(ctx)
		if err != nil {
			return riotBrowserDiscovery{}, fmt.Errorf("watch Riot browser discovery: %w", err)
		}
		switch event.Method {
		case "Network.requestWillBeSent":
			var params struct {
				RequestID string `json:"requestId"`
				FrameID   string `json:"frameId"`
				LoaderID  string `json:"loaderId"`
				Request   struct {
					URL    string `json:"url"`
					Method string `json:"method"`
				} `json:"request"`
			}
			if json.Unmarshal(event.Params, &params) != nil {
				continue
			}
			endpoint, hasQuery := riotBrowserLoginEndpoint(params.Request.URL)
			if !endpoint {
				continue
			}
			if params.Request.Method != http.MethodGet || hasQuery || !isRiotBrowserLoginURL(params.Request.URL) || params.RequestID == "" || params.FrameID == "" || params.LoaderID == "" {
				return riotBrowserDiscovery{}, errors.New("watch Riot browser discovery: invalid initial login request")
			}
			if _, duplicate := requests[params.RequestID]; duplicate {
				return riotBrowserDiscovery{}, errors.New("watch Riot browser discovery: duplicate request identity")
			}
			requests[params.RequestID] = riotBrowserRequest{method: params.Request.Method, rawURL: params.Request.URL, frameID: params.FrameID, loaderID: params.LoaderID}
			log.Printf("Riot browser login request started method=%s query=false", params.Request.Method)
		case "Network.responseReceived":
			var params struct {
				RequestID string `json:"requestId"`
				FrameID   string `json:"frameId"`
				LoaderID  string `json:"loaderId"`
				Response  struct {
					URL    string `json:"url"`
					Status int    `json:"status"`
				} `json:"response"`
			}
			if json.Unmarshal(event.Params, &params) != nil {
				continue
			}
			request, ok := requests[params.RequestID]
			if !ok {
				continue
			}
			if params.FrameID != request.frameID || params.LoaderID != request.loaderID || params.Response.URL != request.rawURL {
				return riotBrowserDiscovery{}, errors.New("watch Riot browser discovery: response identity changed")
			}
			request.response = true
			request.status = params.Response.Status
			requests[params.RequestID] = request
		case "Network.loadingFinished":
			var params struct {
				RequestID string `json:"requestId"`
			}
			if json.Unmarshal(event.Params, &params) != nil {
				continue
			}
			request, ok := requests[params.RequestID]
			if !ok {
				continue
			}
			delete(requests, params.RequestID)
			if !request.response || request.status != http.StatusOK {
				return riotBrowserDiscovery{}, fmt.Errorf("watch Riot browser discovery: rejected status=%d", request.status)
			}
			identity, identityErr := c.riotBrowserDocumentIdentity(ctx)
			if identityErr != nil {
				return riotBrowserDiscovery{}, identityErr
			}
			if identity.frameID != request.frameID || identity.loaderID != request.loaderID {
				return riotBrowserDiscovery{}, errors.New("watch Riot browser discovery: document identity changed")
			}
			body, bodyErr := c.riotResponseBody(ctx, params.RequestID)
			if bodyErr != nil {
				return riotBrowserDiscovery{}, fmt.Errorf("watch Riot browser discovery: %w", bodyErr)
			}
			captcha, summaryErr := strictRiotBrowserDiscoveryCaptcha(body)
			if summaryErr != nil {
				return riotBrowserDiscovery{}, summaryErr
			}
			log.Printf("Riot browser login discovery response method=GET type=%q captcha=%t", "auth", captcha)
			return riotBrowserDiscovery{captcha: captcha}, nil
		}
	}
}

func strictRiotBrowserDiscoveryCaptcha(body []byte) (bool, error) {
	if len(body) == 0 || len(body) > riotBrowserResponseLimit {
		return false, errors.New("watch Riot browser discovery: invalid response body")
	}
	var response struct {
		Type    string          `json:"type"`
		Captcha json.RawMessage `json:"captcha"`
	}
	if json.Unmarshal(body, &response) != nil || response.Type != "auth" {
		return false, errors.New("watch Riot browser discovery: unexpected response type")
	}
	trimmed := bytes.TrimSpace(response.Captcha)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte("false")) {
		return false, nil
	}
	var captcha struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(trimmed, &captcha) != nil || !strings.EqualFold(captcha.Type, "hcaptcha") {
		return false, errors.New("watch Riot browser discovery: unsupported CAPTCHA configuration")
	}
	return true, nil
}

const riotBrowserSubmitTargetExpression = `(function(requiresCaptcha){
if(location.origin!=="https://authenticate.riotgames.com")return {originOK:false,ready:false};
if(document.readyState!=='complete')return {originOK:true,ready:false};
const curtain=window[Symbol.for('valorant.remote-captcha-curtain')];if(!curtain)return {originOK:true,terminal:true,reason:'curtain unavailable'};
const key=Symbol.for('valorant.riot-login-submit-state');
const randomToken=()=>{const bytes=new Uint8Array(16);crypto.getRandomValues(bytes);return Array.from(bytes,b=>b.toString(16).padStart(2,'0')).join('')};
let state=window[key];if(!state){state={documentToken:randomToken(),generation:1,clicked:false,button:null,widget:null,legal:null,api:null,ids:new WeakMap(),nextID:1};Object.defineProperty(window,key,{value:state,configurable:false,writable:false})}
const identity=value=>{let id=state.ids.get(value);if(!id){id=String(state.nextID++);state.ids.set(value,id)}return id};
const roots=[document];for(let i=0;i<roots.length;i++){for(const element of roots[i].querySelectorAll('*')){if(element.shadowRoot)roots.push(element.shadowRoot)}}
const unique=values=>Array.from(new Set(values));
const passwords=unique(roots.flatMap(root=>Array.from(root.querySelectorAll('input[name="password"],input[autocomplete="current-password"],input[data-testid*="password"],input[type="password"]'))));
const buttons=unique(passwords.flatMap(password=>password.form?Array.from(password.form.querySelectorAll('button[data-testid="btn-signin-submit"]')):[]));
if(buttons.length>1)return {originOK:true,terminal:true,reason:'ambiguous submit button'};
if(buttons.length===0)return {originOK:true,ready:false};
const button=buttons[0];if(button.disabled||button.getAttribute('aria-disabled')==='true')return {originOK:true,terminal:true,disabled:true,reason:'submit button disabled'};
let widget=null,legal=null,api=null;
if(requiresCaptcha){
  const legalMarkers=unique(roots.flatMap(root=>Array.from(root.querySelectorAll('[data-testid="hcaptcha-legal"]'))));
  if(legalMarkers.length>1)return {originOK:true,terminal:true,reason:'ambiguous hCaptcha legal marker'};
  const markedFrames=unique(roots.flatMap(root=>Array.from(root.querySelectorAll('iframe[data-hcaptcha-widget-id]'))));
  if(markedFrames.length>1)return {originOK:true,terminal:true,reason:'ambiguous hCaptcha widget'};
  if(legalMarkers.length===0||markedFrames.length===0||!window.hcaptcha||typeof window.hcaptcha.execute!=='function')return {originOK:true,ready:false};
  widget=markedFrames[0];legal=legalMarkers[0];api=window.hcaptcha;
  let parsed;try{parsed=new URL(widget.src,location.href)}catch(_){return {originOK:true,terminal:true,reason:'invalid hCaptcha widget URL'}}
  const host=parsed.hostname.toLowerCase();if(parsed.protocol!=='https:'||!(host==='hcaptcha.com'||host.endsWith('.hcaptcha.com')))return {originOK:true,terminal:true,reason:'invalid hCaptcha widget origin'};
  if(!widget.getAttribute('data-hcaptcha-widget-id'))return {originOK:true,terminal:true,reason:'missing hCaptcha widget identity'};
}
if(state.button!==button||state.widget!==widget||state.legal!==legal||state.api!==api){state.generation++;state.button=button;state.widget=widget;state.legal=legal;state.api=api}
return {originOK:true,ready:true,documentToken:state.documentToken,generation:state.generation,buttonIdentity:identity(button),widgetIdentity:widget?identity(widget):'',legalIdentity:legal?identity(legal):'',apiIdentity:api?identity(api):''};
})`

func (c *chromeDevToolsClient) evaluateRiotBrowserSubmitTarget(ctx context.Context, requiresCaptcha bool) (riotBrowserSubmitTarget, bool, error) {
	expression := riotBrowserSubmitTargetExpression + "(" + strconv.FormatBool(requiresCaptcha) + ")"
	var evaluated struct {
		Result struct {
			Value struct {
				OriginOK       bool   `json:"originOK"`
				Ready          bool   `json:"ready"`
				Terminal       bool   `json:"terminal"`
				Disabled       bool   `json:"disabled"`
				Reason         string `json:"reason"`
				DocumentToken  string `json:"documentToken"`
				Generation     uint64 `json:"generation"`
				ButtonIdentity string `json:"buttonIdentity"`
				WidgetIdentity string `json:"widgetIdentity"`
				LegalIdentity  string `json:"legalIdentity"`
				APIIdentity    string `json:"apiIdentity"`
			} `json:"value"`
		} `json:"result"`
		ExceptionDetails json.RawMessage `json:"exceptionDetails"`
	}
	if err := c.Call(ctx, "Runtime.evaluate", map[string]any{"expression": expression, "returnByValue": true, "awaitPromise": true}, &evaluated); err != nil {
		return riotBrowserSubmitTarget{}, false, err
	}
	value := evaluated.Result.Value
	if len(evaluated.ExceptionDetails) != 0 {
		return riotBrowserSubmitTarget{}, false, errors.New("inspect Riot browser submit target: evaluation failed")
	}
	if !value.OriginOK {
		return riotBrowserSubmitTarget{}, false, errors.New("inspect Riot browser submit target: exact authentication origin changed")
	}
	if value.Terminal {
		if strings.TrimSpace(value.Reason) == "" {
			value.Reason = "invalid submit target"
		}
		return riotBrowserSubmitTarget{}, false, fmt.Errorf("inspect Riot browser submit target: %s", value.Reason)
	}
	if !value.Ready {
		return riotBrowserSubmitTarget{}, false, nil
	}
	target := riotBrowserSubmitTarget{token: value.DocumentToken, generation: value.Generation, buttonID: value.ButtonIdentity,
		widgetID: value.WidgetIdentity, legalID: value.LegalIdentity, apiID: value.APIIdentity, captcha: requiresCaptcha}
	if target.token == "" || target.generation == 0 || target.buttonID == "" ||
		(requiresCaptcha && (target.widgetID == "" || target.legalID == "" || target.apiID == "")) {
		return riotBrowserSubmitTarget{}, false, errors.New("inspect Riot browser submit target: incomplete identity")
	}
	return target, true, nil
}

func sameRiotBrowserSubmitTarget(a, b riotBrowserSubmitTarget) bool {
	return a.document == b.document && a.token != "" && a.token == b.token && a.generation != 0 && a.generation == b.generation &&
		a.buttonID == b.buttonID && a.widgetID == b.widgetID && a.legalID == b.legalID && a.apiID == b.apiID && a.captcha == b.captcha
}

func (c *chromeDevToolsClient) waitForStableRiotBrowserSubmitTarget(ctx context.Context, requiresCaptcha bool) (riotBrowserSubmitTarget, error) {
	var previous riotBrowserSubmitTarget
	havePrevious := false
	navigationRetries := 0
	for {
		before, err := c.riotBrowserDocumentIdentity(ctx)
		if err != nil {
			return riotBrowserSubmitTarget{}, err
		}
		target, ready, evalErr := c.evaluateRiotBrowserSubmitTarget(ctx, requiresCaptcha)
		if evalErr != nil {
			if !retryRiotBrowserNavigationError(evalErr, &navigationRetries) {
				return riotBrowserSubmitTarget{}, fmt.Errorf("inspect Riot browser submit target: %w", evalErr)
			}
			havePrevious = false
			if waitErr := waitRiotBrowserDiscovery(ctx); waitErr != nil {
				return riotBrowserSubmitTarget{}, waitErr
			}
			continue
		}
		after, err := c.riotBrowserDocumentIdentity(ctx)
		if err != nil {
			return riotBrowserSubmitTarget{}, err
		}
		if before != after {
			return riotBrowserSubmitTarget{}, errors.New("inspect Riot browser submit target: document identity changed")
		}
		if ready {
			target.document = after
			if havePrevious && previous.document != target.document {
				return riotBrowserSubmitTarget{}, errors.New("inspect Riot browser submit target: document identity changed")
			}
			if havePrevious && sameRiotBrowserSubmitTarget(previous, target) {
				return target, nil
			}
			previous = target
			havePrevious = true
		} else {
			havePrevious = false
		}
		if waitErr := waitRiotBrowserDiscovery(ctx); waitErr != nil {
			return riotBrowserSubmitTarget{}, waitErr
		}
	}
}

func (c *chromeDevToolsClient) clickRiotBrowserSubmitTarget(ctx context.Context, target riotBrowserSubmitTarget) error {
	identity, err := c.riotBrowserDocumentIdentity(ctx)
	if err != nil {
		return err
	}
	if identity != target.document {
		return errors.New("submit Riot browser login: document identity changed")
	}
	tokenJSON, _ := json.Marshal(target.token)
	buttonJSON, _ := json.Marshal(target.buttonID)
	widgetJSON, _ := json.Marshal(target.widgetID)
	legalJSON, _ := json.Marshal(target.legalID)
	apiJSON, _ := json.Marshal(target.apiID)
	expression := `(function(){
if(location.origin!=="https://authenticate.riotgames.com")return {originOK:false,submitted:false};
const expectedDocumentToken=` + string(tokenJSON) + `,expectedGeneration=` + strconv.FormatUint(target.generation, 10) + `,expectedButtonIdentity=` + string(buttonJSON) + `,expectedWidgetIdentity=` + string(widgetJSON) + `,expectedLegalIdentity=` + string(legalJSON) + `,expectedAPIIdentity=` + string(apiJSON) + `;
const state=window[Symbol.for('valorant.riot-login-submit-state')],curtain=window[Symbol.for('valorant.remote-captcha-curtain')];
if(!state||!curtain||state.clicked||state.documentToken!==expectedDocumentToken||state.generation!==expectedGeneration)return {originOK:true,submitted:false,stale:true};
const identity=value=>state.ids.get(value)||'';const button=state.button;
if(!button||identity(button)!==expectedButtonIdentity||button.disabled||button.getAttribute('aria-disabled')==='true')return {originOK:true,submitted:false,stale:true};
const roots=[document];for(let i=0;i<roots.length;i++){for(const element of roots[i].querySelectorAll('*')){if(element.shadowRoot)roots.push(element.shadowRoot)}}
const unique=values=>Array.from(new Set(values));
const passwords=unique(roots.flatMap(root=>Array.from(root.querySelectorAll('input[name="password"],input[autocomplete="current-password"],input[data-testid*="password"],input[type="password"]'))));
const buttons=unique(passwords.flatMap(password=>password.form?Array.from(password.form.querySelectorAll('button[data-testid="btn-signin-submit"]')):[]));
if(buttons.length!==1||buttons[0]!==button)return {originOK:true,submitted:false,stale:true};
if(expectedWidgetIdentity){
  const widget=state.widget,legal=state.legal,api=state.api;
  const widgets=unique(roots.flatMap(root=>Array.from(root.querySelectorAll('iframe[data-hcaptcha-widget-id]'))));
  const legalMarkers=unique(roots.flatMap(root=>Array.from(root.querySelectorAll('[data-testid="hcaptcha-legal"]'))));
  let parsed;try{parsed=new URL(widget&&widget.src,location.href)}catch(_){return {originOK:true,submitted:false,stale:true}}
  const host=parsed.hostname.toLowerCase();
  if(widgets.length!==1||widgets[0]!==widget||legalMarkers.length!==1||legalMarkers[0]!==legal||!widget.getAttribute('data-hcaptcha-widget-id')||parsed.protocol!=='https:'||!(host==='hcaptcha.com'||host.endsWith('.hcaptcha.com'))||!api||identity(widget)!==expectedWidgetIdentity||identity(legal)!==expectedLegalIdentity||identity(api)!==expectedAPIIdentity||window.hcaptcha!==api||typeof api.execute!=='function'||!widget.isConnected||!legal.isConnected)return {originOK:true,submitted:false,stale:true};
}
state.clicked=true;curtain.trustedSubmit=true;
try{button.click();return {originOK:true,submitted:true}}finally{curtain.trustedSubmit=false}
})()`
	var evaluated struct {
		Result struct {
			Value struct {
				OriginOK  bool `json:"originOK"`
				Submitted bool `json:"submitted"`
				Stale     bool `json:"stale"`
			} `json:"value"`
		} `json:"result"`
		ExceptionDetails json.RawMessage `json:"exceptionDetails"`
	}
	if err := c.Call(ctx, "Runtime.evaluate", map[string]any{"expression": expression, "returnByValue": true, "awaitPromise": true}, &evaluated); err != nil {
		// This evaluation owns the one-shot click. Retrying after a navigation
		// abort could submit a replacement document twice.
		return fmt.Errorf("submit Riot browser login: %w", err)
	}
	if len(evaluated.ExceptionDetails) != 0 || !evaluated.Result.Value.OriginOK || !evaluated.Result.Value.Submitted || evaluated.Result.Value.Stale {
		return errors.New("submit Riot browser login: guarded target changed")
	}
	return nil
}

type riotBrowserRequest struct {
	method   string
	rawURL   string
	frameID  string
	loaderID string
	response bool
	status   int
}

func (c *chromeDevToolsClient) waitForRiotLogin(ctx context.Context, events *chromeDevToolsEventSubscription, publishCaptcha func(riotCaptchaSurface)) (riotBrowserLoginResult, error) {
	return c.waitForRiotLoginState(ctx, events, publishCaptcha, true)
}

func (c *chromeDevToolsClient) waitForRiotLoginState(ctx context.Context, events *chromeDevToolsEventSubscription, publishCaptcha func(riotCaptchaSurface), waitForChallengeSurface bool) (riotBrowserLoginResult, error) {
	requests := make(map[string]riotBrowserRequest)
	seenLoginRequests := make(map[string]struct{})
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
				if endpoint, _ := riotBrowserLoginEndpoint(params.Request.URL); endpoint {
					if _, duplicate := seenLoginRequests[params.RequestID]; duplicate || params.RequestID == "" {
						return riotBrowserLoginResult{}, errors.New("watch Riot browser login: duplicate request identity")
					}
					seenLoginRequests[params.RequestID] = struct{}{}
				}
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
			if endpoint && request.method != http.MethodPut {
				return riotBrowserLoginResult{}, errors.New("watch Riot browser login: duplicate discovery after submit")
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
				if !waitForChallengeSurface {
					continue
				}
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
