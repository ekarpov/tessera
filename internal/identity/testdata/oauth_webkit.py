"""Real WebKit CSP checks against a disposable Go identity authority.

No CSP bypass, request interception or mocked responses. Linux WebKit cannot
prove iOS app dispatch; the custom-scheme test proves the completion document
and checks the clicked link for CSP violations, not OS callback delivery.
"""
import base64
import hashlib
import sys
from urllib.parse import urlencode, urlparse, parse_qs

from playwright.sync_api import sync_playwright, expect

base, dataset = sys.argv[1:]
verifier = "x" * 43
challenge = base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).decode().rstrip("=")

with sync_playwright() as p:
    browser = p.webkit.launch()
    print("WebKit", browser.version)
    for decision in ("cancel", "deny", "allow"):
        context = browser.new_context()
        # Observe CSP violations even on the document that is about to leave.
        violations = []
        context.expose_binding("recordCSP", lambda source, directive: violations.append(directive))
        context.add_init_script("""document.addEventListener('securitypolicyviolation', e => { window.testCSPSeen = true; window.recordCSP(e.effectiveDirective); });""")
        page = context.new_page()
        callback = "com.example.synthetic:/oauth/callback?keep=one"
        registered = context.request.post(base + "/oauth/register", data={"client_name": "Synthetic Notes", "redirect_uris": [callback]})
        assert registered.status == 201
        client = registered.json()["client_id"]
        query = dict(client_id=client, redirect_uri=callback, response_type="code", code_challenge_method="S256", code_challenge=challenge, state="synthetic-state", dataset=dataset)
        response = page.goto(base + "/oauth/authorize?" + urlencode(query))
        assert response.status == 200
        policy = response.headers["content-security-policy"]
        assert "form-action 'self';" in policy and "com.example.synthetic:" not in policy
        if decision == "cancel":
            # A negative control proves WebKit is enforcing the policy and our
            # observer works. An injected external form action must be blocked.
            page.locator("form").evaluate("(f, target) => f.action = target", callback)
            page.locator('button[value=deny]').click(no_wait_after=True)
            # Process real browser events; do not evaluate a polling expression
            # that could itself trigger the CSP observer's script-src branch.
            for _ in range(600):
                if violations:
                    break
                page.wait_for_timeout(50)
            assert violations == ["form-action"], violations
            page.locator("form").evaluate("f => f.action = '/oauth/authorize'")
            violations.clear()
        if decision != "cancel":
            page.locator('[name=username]').fill("person")
            page.locator('[name=password]').fill("synthetic-password-only")
            page.locator('button[value=continue]').click()
            expect(page.locator('body')).to_have_attribute("data-page", "consent")
        form = page.locator("form").evaluate("f => Object.fromEntries(new FormData(f))")
        action = "allow" if decision == "allow" else "deny"
        with page.expect_response(lambda r: r.url == base + "/oauth/authorize" and r.request.method == "POST") as completed:
            page.locator(f'button[value={action}]').click()
        response = completed.value
        assert response.status == 200
        assert response.headers["content-security-policy"] == policy
        assert response.headers["cache-control"] == "no-store"
        assert response.headers["referrer-policy"] == "no-referrer"
        expect(page.locator("body")).to_have_attribute("data-page", "complete")
        link = page.get_by_role("link", name="Return to Synthetic Notes")
        target = link.get_attribute("href")
        parsed = urlparse(target)
        params = parse_qs(parsed.query)
        assert parsed.scheme == "com.example.synthetic" and parsed.path == "/oauth/callback"
        assert params["state"] == ["synthetic-state"] and params["iss"] == [base] and params["keep"] == ["one"]
        assert page.locator("form,script").count() == 0
        form["decision"] = action
        if decision == "allow":
            exchange = dict(grant_type="authorization_code", client_id=client, redirect_uri=callback, code=params["code"][0], code_verifier="y" * 43)
            assert context.request.post(base + "/oauth/token", form=exchange).status == 400
            exchange["code_verifier"] = verifier
            assert context.request.post(base + "/oauth/token", form=exchange).status == 200
            assert context.request.post(base + "/oauth/token", form=exchange).status == 400
        else:
            assert params["error"] == ["access_denied"] and "code" not in params
        # The synthetic scheme has no OS handler here. The navigation may fail;
        # it must not be refused by CSP. Keep the source document for inspection.
        link.click(no_wait_after=True)
        page.wait_for_timeout(300)
        assert violations == [], violations
        # Submit the same form again through WebKit, including after redemption.
        # This exercises document navigation under the real page CSP.
        with page.expect_response(lambda r: r.url == base + "/oauth/authorize" and r.request.method == "POST") as duplicate:
            page.evaluate("""values => {
                const f = document.createElement('form');
                f.method = 'post'; f.action = '/oauth/authorize';
                for (const [name, value] of Object.entries(values)) {
                    const input = document.createElement('input');
                    input.type = 'hidden'; input.name = name; input.value = value;
                    f.appendChild(input);
                }
                document.body.appendChild(f); f.requestSubmit();
            }""", form)
        assert duplicate.value.status == 400
        expect(page.locator('body')).to_have_attribute("data-page", "restart")
        restart = page.get_by_role("link", name="Start again")
        restart_uri = urlparse(restart.get_attribute("href"))
        assert restart_uri.scheme == "" and restart_uri.netloc == "" and restart_uri.path == "/oauth/authorize"
        assert parse_qs(restart_uri.query) == {key: [value] for key, value in query.items()}
        assert "code=" not in page.content()
        restart.click()
        expect(page.locator('body')).to_have_attribute("data-page", "signin")
        assert violations == [], violations
        print(decision + ": real form submission, completion link, duplicate recovery, fresh sign-in and PKCE passed")
        context.close()
    browser.close()
