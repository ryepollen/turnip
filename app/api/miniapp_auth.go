package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	log "github.com/go-pkgz/lgr"
	"github.com/go-pkgz/rest"
)

// initDataHeader carries the Telegram Mini App initData string. It goes in a
// header (not the URL) so the signed payload never lands in access logs.
const initDataHeader = "X-Telegram-Init-Data"

// tgUser is the subset of the Telegram Mini App user object we care about
type tgUser struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

// verifyInitData validates a Telegram Mini App initData string against the bot
// token and returns the embedded user. The algorithm is Telegram's: build a
// data-check-string from all fields except hash (sorted, key=value, joined by
// \n), derive secret = HMAC_SHA256("WebAppData", token), and compare
// HMAC_SHA256(secret, data-check-string) with the provided hash. maxAge bounds
// how stale auth_date may be (0 disables the freshness check). Pure and
// side-effect free so it can be unit-tested without a server.
func verifyInitData(initData, botToken string, maxAge time.Duration, now time.Time) (tgUser, error) {
	if botToken == "" {
		return tgUser{}, fmt.Errorf("bot token not configured")
	}
	values, err := url.ParseQuery(initData)
	if err != nil {
		return tgUser{}, fmt.Errorf("bad initData: %w", err)
	}
	hash := values.Get("hash")
	if hash == "" {
		return tgUser{}, fmt.Errorf("initData has no hash")
	}

	// data-check-string: every field except hash, "key=value", sorted by key,
	// joined with newlines. url.ParseQuery already url-decoded the values.
	keys := make([]string, 0, len(values))
	for k := range values {
		if k == "hash" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, k+"="+values.Get(k))
	}
	dataCheckString := strings.Join(pairs, "\n")

	secret := hmacSHA256([]byte("WebAppData"), []byte(botToken))
	calc := hmacSHA256(secret, []byte(dataCheckString))
	want, err := hex.DecodeString(hash)
	if err != nil {
		return tgUser{}, fmt.Errorf("bad hash encoding: %w", err)
	}
	if !hmac.Equal(calc, want) {
		return tgUser{}, fmt.Errorf("initData signature mismatch")
	}

	if maxAge > 0 {
		authUnix, aerr := strconv.ParseInt(values.Get("auth_date"), 10, 64)
		if aerr != nil {
			return tgUser{}, fmt.Errorf("bad auth_date: %w", aerr)
		}
		if age := now.Sub(time.Unix(authUnix, 0)); age > maxAge {
			return tgUser{}, fmt.Errorf("initData expired (%s old)", age.Round(time.Second))
		}
	}

	var user tgUser
	if uerr := json.Unmarshal([]byte(values.Get("user")), &user); uerr != nil {
		return tgUser{}, fmt.Errorf("bad user field: %w", uerr)
	}
	if user.ID == 0 {
		return tgUser{}, fmt.Errorf("initData has no user id")
	}
	return user, nil
}

// hmacSHA256 returns HMAC-SHA256(message) keyed by key
func hmacSHA256(key, message []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(message)
	return m.Sum(nil)
}

// initDataMaxAge bounds how old a Mini App session may be. Generous: a Mini App
// tab can stay open for a long time and we don't want to nag; the signature and
// the single-user gate are the real guards.
const initDataMaxAge = 48 * time.Hour

// miniAuth gates the Mini App API to the one allowed user. It verifies the
// initData signature with the bot token and checks the user id, answering 401
// JSON on any failure. The static shell is served without this — data lives
// only behind here.
func (s *Server) miniAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, err := verifyInitData(r.Header.Get(initDataHeader), s.BotToken, initDataMaxAge, time.Now())
		if err != nil {
			rest.SendErrorJSON(w, r, log.Default(), http.StatusUnauthorized, err, "unauthorized")
			return
		}
		if s.AllowedUserID != 0 && user.ID != s.AllowedUserID {
			rest.SendErrorJSON(w, r, log.Default(), http.StatusUnauthorized,
				fmt.Errorf("user %d not allowed", user.ID), "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}
