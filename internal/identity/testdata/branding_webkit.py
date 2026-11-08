"""Real WebKit branding/layout/keyboard checks; no mocked routes or CSP bypass."""
import base64
import hashlib
import os
import sys
from urllib.parse import urlencode, urlparse, parse_qs
from playwright.sync_api import sync_playwright, expect

base, dataset, callback, width, theme, kind, screenshots = sys.argv[1:]
verifier = "x" * 43
challenge = base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).decode().rstrip("=")
query = dict(client_id="console", redirect_uri=callback, response_type="code", code_challenge_method="S256", code_challenge=challenge, state="synthetic-state", dataset=dataset, scope="read write")

def check_page(page, stage):
    expect(page.locator("header strong")).to_have_text("Synthetic Home")
    expect(page.locator("header")).to_contain_text(urlparse(base).netloc)
    image = page.locator("header img")
    expect(image).to_have_attribute("alt", "")  # Adjacent visible site name supplies identity.
    # Poll the DOM property without eval-based wait_for_function: the page's
    # strict CSP must apply to page scripts, including eval attempted by a driver.
    expect(image).to_have_js_property("complete", True, timeout=30000)
    assert image.evaluate("element => element.naturalWidth") > 0, stage
    assert page.evaluate("document.documentElement.scrollWidth <= window.innerWidth"), stage
    assert page.locator("script").count() == 0
    # Check actual rendered colors, rather than just configured values.
    ratio = page.evaluate("""() => {
        const rgb = c => c.match(/[0-9.]+/g).slice(0,3).map(Number).map(v => {
            v/=255; return v<=.04045 ? v/12.92 : ((v+.055)/1.055)**2.4;
        });
        const lum = c => rgb(c).reduce((sum,v,i) => sum+v*[.2126,.7152,.0722][i],0);
        const ratio = (a,b) => (Math.max(lum(a),lum(b))+.05)/(Math.min(lum(a),lum(b))+.05);
        const body = getComputedStyle(document.body);
        const button = document.querySelector('button,.button');
        const answer = [ratio(body.color,body.backgroundColor)];
        if(button) { const s=getComputedStyle(button); answer.push(ratio(s.color,s.backgroundColor)); }
        return answer;
    }""")
    assert min(ratio) >= 4.5, (stage, ratio)

with sync_playwright() as p:
    browser = p.webkit.launch()
    for decision in ("cancel", "deny", "allow"):
        context = browser.new_context(ignore_https_errors=True, viewport={"width": int(width), "height": 900}, color_scheme=theme)
        violations = []
        context.expose_binding("recordBrandCSP", lambda source, event: violations.append(event))
        context.add_init_script("document.addEventListener('securitypolicyviolation', e => window.recordBrandCSP({directive:e.effectiveDirective,uri:e.blockedURI,source:e.sourceFile,line:e.lineNumber,sample:e.sample}))")
        page = context.new_page()
        response = page.goto(base + "/oauth/authorize?" + urlencode(query))
        assert response.status == 200
        policy = response.headers["content-security-policy"]
        assert "default-src 'none'" in policy and "form-action 'self'" in policy and "frame-ancestors 'none'" in policy
        assert "img-src " + base + "/oauth/brand/logo" in policy
        assert "unsafe-inline" not in policy and "script-src" not in policy
        check_page(page, "signin")
        expect(page.get_by_role("heading", name="Sign in to Synthetic Home")).to_be_visible()
        expect(page.locator('[name=username]')).to_have_attribute("autocomplete", "username")
        expect(page.locator('[name=password]')).to_have_attribute("autocomplete", "current-password")
        assert ("designated by the server operator" in page.content()) == (kind == "web")
        # Keyboard-only focus sequence, with visible focus styling.
        page.keyboard.press("Tab")
        expect(page.locator('[name=username]')).to_be_focused()
        page.keyboard.type("person")
        page.keyboard.press("Tab")
        expect(page.locator('[name=password]')).to_be_focused()
        outline = page.locator('[name=password]').evaluate("e => getComputedStyle(e).outlineStyle")
        assert outline != "none"
        if decision == "cancel":
            page.keyboard.press("Tab")
            page.keyboard.press("Tab")
            expect(page.locator('button[value=deny]')).to_be_focused()
            page.keyboard.press("Enter")
        else:
            page.keyboard.type("synthetic-password-only")
            if decision == "allow":
                # A credential error remains branded and keeps the same flow.
                page.locator('[name=password]').fill("wrong")
                page.locator('button[value=continue]').focus()
                page.keyboard.press("Enter")
                expect(page.locator('[role=alert]')).to_be_visible()
                check_page(page, "wrong-password")
                page.locator('[name=username]').fill("person")
                page.locator('[name=password]').fill("synthetic-password-only")
                page.locator('[name=password]').focus()
            page.keyboard.press("Tab")
            expect(page.locator('button[value=continue]')).to_be_focused()
            page.keyboard.press("Enter")
            expect(page.locator('body')).to_have_attribute("data-page", "consent")
            check_page(page, "consent")
            expect(page.locator('[name=read]')).to_be_checked()
            expect(page.locator('[name=write]')).to_be_checked()
            for name in ("schema", "erase", "insert", "administer"):
                if page.locator(f'[name={name}]').count():
                    expect(page.locator(f'[name={name}]')).not_to_be_checked()
            # Toggle a permission using Space and restore it, then reach the
            # actual consent action using Tab, with no pointer submission.
            page.keyboard.press("Tab")
            expect(page.locator('details > summary')).to_be_focused()
            expect(page.locator('details')).not_to_have_attribute("open", "")
            page.keyboard.press("Space")
            page.keyboard.press("Tab")
            expect(page.locator('[name=read]')).to_be_focused()
            page.keyboard.press("Space")
            expect(page.locator('[name=read]')).not_to_be_checked()
            page.keyboard.press("Space")
            action = "allow" if decision == "allow" else "deny"
            for _ in range(12):
                if page.locator(f'button[value={action}]').evaluate("e => e === document.activeElement"):
                    break
                page.keyboard.press("Tab")
            expect(page.locator(f'button[value={action}]')).to_be_focused()
            form = page.locator("form").evaluate("f => Object.fromEntries(new FormData(f))")
            form["decision"] = action
            page.keyboard.press("Enter")
        expect(page.locator('body')).to_have_attribute("data-page", "complete")
        check_page(page, "complete-" + decision)
        expect(page.get_by_role("heading", name="Return to website" if kind == "web" else "Return to app")).to_be_visible()
        link = page.get_by_role("link", name="Return to Synthetic Console")
        target = link.get_attribute("href")
        params = parse_qs(urlparse(target).query)
        assert params["state"] == [query["state"]] and params["iss"] == [base]
        if decision == "allow":
            exchange = dict(grant_type="authorization_code", client_id="console", redirect_uri=callback, code=params["code"][0], code_verifier=verifier)
            assert context.request.post(base + "/oauth/token", form=exchange).status == 200
            assert context.request.post(base + "/oauth/token", form=exchange).status == 400
            # Replay produces a branded recovery page. Follow it and confirm
            # that authentication and consent are required again, with hints.
            with page.expect_response(lambda r: r.url == base + "/oauth/authorize" and r.request.method == "POST") as replayed:
                page.evaluate("""values => {
                    const f=document.createElement('form'); f.method='post'; f.action='/oauth/authorize';
                    for(const [name,value] of Object.entries(values)) {
                        const i=document.createElement('input'); i.type='hidden'; i.name=name; i.value=value; f.appendChild(i);
                    }
                    document.body.appendChild(f); f.requestSubmit();
                }""", form)
            assert replayed.value.status == 400 and replayed.value.headers["content-security-policy"] == policy
            expect(page.locator('body')).to_have_attribute("data-page", "restart")
            restart = page.get_by_role("link", name="Start again").get_attribute("href")
            assert parse_qs(urlparse(restart).query) == {key: [value] for key,value in query.items()}
            check_page(page, "restart")
            page.get_by_role("link", name="Start again").focus()
            page.keyboard.press("Enter")
            expect(page.locator('body')).to_have_attribute("data-page", "signin")
            check_page(page, "recovery-signin")
            # Completion link is still an explicit navigation, never a form
            # redirect. HTTPS lands on a real synthetic callback route.
            page.goto(base + "/oauth/authorize?" + urlencode(query))
            if kind == "web":
                page.goto(target)
                expect(page.locator("body")).to_have_text("Synthetic callback received")
        else:
            assert params["error"] == ["access_denied"] and "code" not in params
            if kind == "web":
                link.focus()
                page.keyboard.press("Enter")
                expect(page.locator("body")).to_have_text("Synthetic callback received")
            else:
                link.focus()
                page.keyboard.press("Enter")
        assert violations == [], violations
        # An invalid callback gets branded errors and never repeats its address.
        bad = dict(query, redirect_uri="https://attacker.invalid/callback")
        response = page.goto(base + "/oauth/authorize?" + urlencode(bad))
        assert response.status == 400
        check_page(page, "invalid-link")
        assert "attacker.invalid" not in page.content()
        context.close()
    if screenshots:
        # Visual QA is separate from CSP acceptance above. Playwright's WebKit
        # screenshot helper inserts a temporary `body {}` style (blocked by our
        # CSP); it must not pollute the acceptance observer or loosen the policy.
        preview = browser.new_context(ignore_https_errors=True, viewport={"width":int(width),"height":900}, color_scheme=theme)
        page = preview.new_page()
        page.goto(base + "/oauth/authorize?" + urlencode(dict(query,prompt="consent")))
        os.makedirs(screenshots, exist_ok=True)
        def snapshot(stage):
            page.screenshot(path=os.path.join(screenshots,f"{kind}-{width}-{theme}-{stage}.png"),full_page=True,caret="initial")
        snapshot("signin")
        page.locator('[name=username]').fill("person")
        page.locator('[name=password]').fill("synthetic-password-only")
        page.locator('button[value=continue]').focus()
        page.keyboard.press("Enter")
        expect(page.locator('body')).to_have_attribute("data-page","consent")
        snapshot("consent")
        page.goto(base + "/oauth/authorize?" + urlencode(dict(query,redirect_uri="https://attacker.invalid/callback")))
        snapshot("error")
        preview.close()
    browser.close()
    print(f"{kind} {width}px {theme}: branding, CSP, contrast, keyboard, autofill attributes, consent, callbacks and recovery passed")
