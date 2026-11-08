"""Single-submit first-party Console under actual WebKit CSP enforcement."""
import base64
import hashlib
import os
import sys
from urllib.parse import urlencode, urlparse, parse_qs
from playwright.sync_api import sync_playwright, expect

base, dataset, console_origin, width, theme, screenshots = sys.argv[1:]
verifier = "x" * 43
challenge = base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).decode().rstrip("=")
q = dict(client_id="operator-console",redirect_uri=console_origin+"/callback",response_type="code",code_challenge_method="S256",code_challenge=challenge,state="bound-console-state")
with sync_playwright() as p:
    browser=p.webkit.launch()
    for remember in (False,True):
        context=browser.new_context(ignore_https_errors=True,viewport={"width":int(width),"height":900},color_scheme=theme)
        violations=[]
        context.expose_binding("consoleCSP",lambda source,directive:violations.append(directive))
        context.add_init_script("document.addEventListener('securitypolicyviolation',e=>window.consoleCSP(e.effectiveDirective))")
        page=context.new_page()
        response=page.goto(base+"/oauth/authorize?"+urlencode(q))
        policy=response.headers["content-security-policy"]
        assert "form-action 'self'" in policy and "default-src 'none'" in policy and "unsafe-inline" not in policy
        expect(page.get_by_role("heading",name="Sign in",exact=True)).to_be_visible()
        expect(page.locator("header strong")).to_have_text("Synthetic Home")
        expect(page.locator('button')).to_have_count(1)
        expect(page.locator('[name=remember]')).not_to_be_checked()
        for forbidden in ("Allow access to your data","Manage accounts for you","Continue to sign in"):
            assert forbidden not in page.content()
        assert page.locator('[name=dataset],[name=read],[name=write],[name=administer]').count()==0
        assert page.evaluate("document.documentElement.scrollWidth <= innerWidth")
        expect(page.locator('[name=username]')).to_have_attribute("autocomplete","username")
        expect(page.locator('[name=password]')).to_have_attribute("autocomplete","current-password")
        logo = page.locator("header img")
        expect(logo).to_have_js_property("complete", True, timeout=30000)
        assert logo.evaluate("element => element.naturalWidth") > 0
        page.keyboard.press("Tab");expect(page.locator('[name=username]')).to_be_focused();page.keyboard.type("person")
        page.keyboard.press("Tab");expect(page.locator('[name=password]')).to_be_focused();page.keyboard.type("synthetic-password-only")
        page.keyboard.press("Tab");expect(page.locator('[name=remember]')).to_be_focused()
        if remember: page.keyboard.press("Space")
        page.keyboard.press("Tab");expect(page.locator('button')).to_be_focused()
        submitted=page.locator("form").evaluate("f=>Object.fromEntries(new FormData(f))")
        submitted["decision"]="continue"
        pages=[]
        page.on("response",lambda r:pages.append(r))
        page.keyboard.press("Enter")
        expect(page).to_have_url(__import__('re').compile(__import__('re').escape(console_origin)+"/callback\\?"))
        expect(page.locator("body")).to_have_text("Synthetic Console signed in")
        # The browser followed a document refresh after same-origin POST 200,
        # with no user click, script or weakened cross-origin form policy.
        completion=[r for r in pages if r.request.method=="POST" and r.url==base+"/oauth/authorize"]
        assert len(completion)==1 and completion[0].status==200
        assert completion[0].headers["content-security-policy"]==policy
        params=parse_qs(urlparse(page.url).query)
        assert params["state"]==[q["state"]] and params["iss"]==[base]
        assert "remember" not in params and "keep_signed_in" not in params
        exchange=dict(grant_type="authorization_code",client_id="operator-console",redirect_uri=q["redirect_uri"],code=params["code"][0],code_verifier=verifier,remember="yes")
        wrong=dict(exchange,code_verifier="y"*43)
        assert context.request.post(base+"/oauth/token",form=wrong).status==400
        tokens=context.request.post(base+"/oauth/token",form=exchange)
        assert tokens.status==200
        payload=tokens.json()
        assert payload["console_session"]==dict(origin=console_origin,keep_signed_in=remember,max_age_seconds=2592000 if remember else 0)
        assert payload["expires_in"]==300
        assert context.request.post(base+"/oauth/token",form=exchange).status==400
        listing=context.request.get(base+"/oauth/account",headers={"Authorization":"Bearer "+payload["access_token"]})
        assert listing.status==200 and listing.json()["installation"]["administer"] is True
        replay=context.request.post(base+"/oauth/authorize",form=submitted,headers={"Accept":"text/html"})
        assert replay.status==400 and 'data-page="restart"' in replay.text()
        assert violations==[],violations
        context.close()
    if screenshots:
        preview=browser.new_context(ignore_https_errors=True,viewport={"width":int(width),"height":900},color_scheme=theme)
        page=preview.new_page();page.goto(base+"/oauth/authorize?"+urlencode(q))
        logo = page.locator("header img")
        expect(logo).to_have_js_property("complete", True, timeout=30000)
        assert logo.evaluate("element => element.naturalWidth") > 0
        os.makedirs(screenshots,exist_ok=True)
        page.screenshot(path=os.path.join(screenshots,f"console-{width}-{theme}-signin.png"),full_page=True,caret="initial")
        preview.close()
    browser.close()
    print(f"Console {width}px {theme}: single-submit callback, keyboard, autofill semantics, unchecked persistence choice, CSP, PKCE, replay and account authority passed")
