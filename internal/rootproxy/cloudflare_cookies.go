package rootproxy

import (
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
)

type chatGPTCloudflareCookieJar struct {
	delegate http.CookieJar
}

func newChatGPTCloudflareCookieJar() (http.CookieJar, error) {
	delegate, errJar := cookiejar.New(nil)
	if errJar != nil {
		return nil, errJar
	}
	return &chatGPTCloudflareCookieJar{delegate: delegate}, nil
}

func (j *chatGPTCloudflareCookieJar) SetCookies(target *url.URL, cookies []*http.Cookie) {
	if j == nil || j.delegate == nil || !isChatGPTCookieURL(target) {
		return
	}
	filtered := make([]*http.Cookie, 0, len(cookies))
	for _, cookie := range cookies {
		if cookie == nil || !isAllowedCloudflareCookieName(cookie.Name) {
			continue
		}
		copyCookie := *cookie
		filtered = append(filtered, &copyCookie)
	}
	if len(filtered) != 0 {
		j.delegate.SetCookies(target, filtered)
	}
}

func (j *chatGPTCloudflareCookieJar) Cookies(target *url.URL) []*http.Cookie {
	if j == nil || j.delegate == nil || !isChatGPTCookieURL(target) {
		return nil
	}
	cookies := j.delegate.Cookies(target)
	filtered := make([]*http.Cookie, 0, len(cookies))
	for _, cookie := range cookies {
		if cookie == nil || !isAllowedCloudflareCookieName(cookie.Name) {
			continue
		}
		copyCookie := *cookie
		filtered = append(filtered, &copyCookie)
	}
	return filtered
}

func isChatGPTCookieURL(target *url.URL) bool {
	if target == nil || !strings.EqualFold(target.Scheme, "https") {
		return false
	}
	host := strings.ToLower(strings.TrimSpace(target.Hostname()))
	return host == "chatgpt.com" || strings.HasSuffix(host, ".chatgpt.com")
}

func isAllowedCloudflareCookieName(name string) bool {
	switch name {
	case "__cf_bm", "__cflb", "__cfruid", "__cfseq", "__cfwaitingroom", "_cfuvid", "cf_clearance", "cf_ob_info", "cf_use_ob":
		return true
	default:
		return strings.HasPrefix(name, "cf_chl_")
	}
}
