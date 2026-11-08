"""Native OAuth pages in a real system-browser engine, never embedded login."""
import base64
import hashlib
import os
import sys
from urllib.parse import urlencode, urlparse, parse_qs
from playwright.sync_api import sync_playwright, expect

base,dataset,width,theme,logo_path,screenshots=sys.argv[1:]
verifier="x"*43
challenge=base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).decode().rstrip("=")
with sync_playwright() as p:
    browser=p.webkit.launch()
    context=browser.new_context(viewport={"width":int(width),"height":900},color_scheme=theme)
    logo=base64.b64encode(open(logo_path,"rb").read()).decode()
    registered=context.request.post(base+"/oauth/register",data=dict(client_name="Native Notes",redirect_uris=["com.example.notes:/callback"],logo_png=logo))
    assert registered.status==201
    client=registered.json()["client_id"]
    query=dict(client_id=client,redirect_uri="com.example.notes:/callback",response_type="code",state="native-state",dataset=dataset,code_challenge_method="S256",code_challenge=challenge)
    violations=[]
    context.expose_binding("nativeCSP",lambda source,directive:violations.append(directive))
    context.add_init_script("document.addEventListener('securitypolicyviolation',e=>window.nativeCSP(e.effectiveDirective))")
    page=context.new_page()
    def sign_in(scope,prompt=None):
        q=dict(query,scope=scope)
        if prompt: q["prompt"]=prompt
        response=page.goto(base+"/oauth/authorize?"+urlencode(q))
        assert response.status==200 and "form-action 'self'" in response.headers["content-security-policy"]
        expect(page.locator("header strong")).to_have_text("Synthetic Home")
        expect(page.locator(".client-icon")).to_be_visible()
        icon = page.locator(".client-icon")
        expect(icon).to_have_js_property("complete", True, timeout=30000)
        assert icon.evaluate("element => element.naturalWidth") > 0
        expect(page.locator("main")).to_contain_text("Native Notes is the name the app gives itself")
        page.locator('[name=username]').fill("person");page.locator('[name=password]').fill("synthetic-password-only")
        page.locator('button[value=continue]').focus();page.keyboard.press("Enter")
    def exchange():
        expect(page.locator("body")).to_have_attribute("data-page","complete")
        expect(page.get_by_role("heading",name="Return to app")).to_be_visible()
        assert page.locator('meta[http-equiv=refresh]').count()==0
        target=page.get_by_role("link",name="Return to Native Notes").get_attribute("href")
        params=parse_qs(urlparse(target).query)
        if "code" in params:
            response=context.request.post(base+"/oauth/token",form=dict(grant_type="authorization_code",client_id=client,redirect_uri=query["redirect_uri"],code=params["code"][0],code_verifier=verifier))
            assert response.status==200 and "console_session" not in response.json()
        else:
            assert params["error"]==["access_denied"]
        page.get_by_role("link",name="Return to Native Notes").focus();page.keyboard.press("Enter")
    sign_in("read write")
    expect(page.locator("body")).to_have_attribute("data-page","consent")
    expect(page.locator("main")).to_contain_text("Native Notes will see your data and add, change and delete entries.")
    assert not page.locator("details").evaluate("e=>e.open")
    expect(page.get_by_role("button",name="Allow",exact=True)).to_be_visible()
    expect(page.get_by_role("button",name="Cancel",exact=True)).to_be_visible()
    page.get_by_text("Change what the app may do",exact=True).focus();page.keyboard.press("Space")
    expect(page.locator('[name=write]')).to_be_checked()
    page.locator('[name=write]').focus();page.keyboard.press("Space")
    expect(page.locator('[name=write]')).not_to_be_checked()
    page.locator('button[value=allow]').focus();page.keyboard.press("Enter");exchange()
    sign_in("read")
    # Exactly the last grant's subset: no consent page, but explicit native completion.
    exchange()
    sign_in("read write")
    expect(page.locator("body")).to_have_attribute("data-page","consent")
    page.locator('button[value=deny]').focus();page.keyboard.press("Enter");exchange()
    sign_in("read","consent")
    expect(page.locator("body")).to_have_attribute("data-page","consent")
    page.locator('button[value=allow]').focus();page.keyboard.press("Enter");exchange()
    sign_in("read schema erase administer")
    expect(page.locator("body")).to_have_attribute("data-page","consent")
    for name in ("schema","erase","administer"):
        expect(page.locator(f'[name={name}]')).not_to_be_checked()
    assert page.evaluate("document.documentElement.scrollWidth<=innerWidth")
    page.locator('button[value=deny]').focus();page.keyboard.press("Enter");exchange()
    assert violations==[],violations
    if screenshots:
        # Separate context avoids screenshot-helper stylesheet attempts entering
        # the actual CSP acceptance observer.
        preview=browser.new_context(viewport={"width":int(width),"height":900},color_scheme=theme)
        page=preview.new_page();page.goto(base+"/oauth/authorize?"+urlencode(dict(query,scope="read write",prompt="consent")))
        page.locator('[name=username]').fill("person");page.locator('[name=password]').fill("synthetic-password-only")
        page.locator('button[value=continue]').focus();page.keyboard.press("Enter")
        expect(page.locator("body")).to_have_attribute("data-page","consent")
        os.makedirs(screenshots,exist_ok=True)
        page.screenshot(path=os.path.join(screenshots,f"native-simple-{width}-{theme}.png"),full_page=True,caret="initial")
        preview.close()
    context.close();browser.close()
    print(f"Native {width}px {theme}: registered icon, collapsed grants, unticked Write, bounded consent reuse, force review and strict CSP passed")
