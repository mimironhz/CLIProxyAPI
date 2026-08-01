package rootproxy

import (
	"net/http"
	"net/url"
	"sort"
	"testing"
)

func TestChatGPTCloudflareCookieJarKeepsOnlyInfrastructureCookies(t *testing.T) {
	jar, errJar := newChatGPTCloudflareCookieJar()
	if errJar != nil {
		t.Fatalf("newChatGPTCloudflareCookieJar() error = %v", errJar)
	}
	target, _ := url.Parse("https://chatgpt.com/backend-api/codex/responses")
	jar.SetCookies(target, []*http.Cookie{
		{Name: "__cflb", Value: "west", Path: "/", Secure: true},
		{Name: "_cfuvid", Value: "visitor", Path: "/", Secure: true},
		{Name: "cf_chl_2", Value: "challenge", Path: "/", Secure: true},
		{Name: "chatgpt_session", Value: "account-secret", Path: "/", Secure: true},
	})

	cookies := jar.Cookies(target)
	names := make([]string, 0, len(cookies))
	for _, cookie := range cookies {
		names = append(names, cookie.Name)
	}
	sort.Strings(names)
	want := []string{"__cflb", "_cfuvid", "cf_chl_2"}
	if len(names) != len(want) {
		t.Fatalf("cookie names = %v, want %v", names, want)
	}
	for index := range want {
		if names[index] != want[index] {
			t.Fatalf("cookie names = %v, want %v", names, want)
		}
	}
}

func TestChatGPTCloudflareCookieJarRejectsOtherOrigins(t *testing.T) {
	jar, errJar := newChatGPTCloudflareCookieJar()
	if errJar != nil {
		t.Fatalf("newChatGPTCloudflareCookieJar() error = %v", errJar)
	}
	for _, rawURL := range []string{
		"http://chatgpt.com/backend-api/codex/responses",
		"https://evilchatgpt.com/backend-api/codex/responses",
		"https://chatgpt.com.evil.example/backend-api/codex/responses",
	} {
		target, _ := url.Parse(rawURL)
		jar.SetCookies(target, []*http.Cookie{{Name: "__cflb", Value: "west", Path: "/", Secure: true}})
		if cookies := jar.Cookies(target); len(cookies) != 0 {
			t.Fatalf("cookies for %s = %#v, want none", rawURL, cookies)
		}
	}
}

func TestWebsocketBridgeUsesFilteredOfficialCookiesOnly(t *testing.T) {
	jar, errJar := newChatGPTCloudflareCookieJar()
	if errJar != nil {
		t.Fatalf("newChatGPTCloudflareCookieJar() error = %v", errJar)
	}
	target, _ := url.Parse("https://chatgpt.com/backend-api/codex/responses")
	jar.SetCookies(target, []*http.Cookie{{Name: "__cflb", Value: "west", Path: "/", Secure: true}})
	bridge := &websocketBridge{officialCookies: jar, officialCookieURL: target}

	officialHeaders := make(http.Header)
	bridge.addOfficialCookies(routeOfficial, officialHeaders)
	if got := officialHeaders.Get("Cookie"); got != "__cflb=west" {
		t.Fatalf("official Cookie = %q, want filtered Cloudflare cookie", got)
	}
	relayHeaders := make(http.Header)
	bridge.addOfficialCookies(routeRelay, relayHeaders)
	if got := relayHeaders.Get("Cookie"); got != "" {
		t.Fatalf("Relay Cookie = %q, want absent", got)
	}

	bridge.storeOfficialCookies(routeOfficial, &http.Response{Header: http.Header{
		"Set-Cookie": {"_cfuvid=visitor; Path=/; Secure", "chatgpt_session=secret; Path=/; Secure"},
	}})
	cookies := jar.Cookies(target)
	for _, cookie := range cookies {
		if cookie.Name == "chatgpt_session" {
			t.Fatal("ChatGPT session cookie entered the shared infrastructure jar")
		}
	}
	if len(cookies) != 2 {
		t.Fatalf("stored cookies = %#v, want __cflb and _cfuvid", cookies)
	}
}
