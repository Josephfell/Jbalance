package admin

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	sessionCookieName = "lb_admin_session"
	sessionDuration   = 12 * time.Hour
)

// issueSession sets a signed session cookie on the response. The cookie
// value is "<expiryUnixSeconds>.<base64url-hmac>" — no session data is
// stored server-side (matching the "no separate database, all local"
// requirement); the signature alone proves the cookie was issued by this
// server and hasn't been tampered with or replayed past its expiry.
func issueSession(w http.ResponseWriter, secret string, secure bool) {
	expiry := time.Now().Add(sessionDuration).Unix()
	sig := signSession(secret, expiry)
	value := fmt.Sprintf("%d.%s", expiry, sig)

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(sessionDuration.Seconds()),
	})
}

// clearSession removes the session cookie (used on logout and password
// reset, in addition to the secret rotation that invalidates old cookies
// server-side).
func clearSession(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}

// validSession reports whether the request carries a session cookie that
// is correctly signed with secret and not expired.
func validSession(r *http.Request, secret string) bool {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}

	parts := strings.SplitN(cookie.Value, ".", 2)
	if len(parts) != 2 {
		return false
	}

	expiry, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return false
	}
	if time.Now().Unix() > expiry {
		return false
	}

	expectedSig := signSession(secret, expiry)
	return hmac.Equal([]byte(parts[1]), []byte(expectedSig))
}

func signSession(secret string, expiry int64) string {
	mac := hmac.New(sha256.New, []byte(secret))
	// hash.Hash.Write never returns an error — Go's documented contract for
	// the interface guarantees this.
	mac.Write([]byte(strconv.FormatInt(expiry, 10)))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
