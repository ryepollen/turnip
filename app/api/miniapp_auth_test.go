package api

import (
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// signInitData builds a valid Telegram initData query string for the given
// fields, signing it exactly as Telegram would with the bot token.
func signInitData(token string, fields map[string]string) string {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	// data-check-string requires sorted keys
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+fields[k])
	}
	secret := hmacSHA256([]byte("WebAppData"), []byte(token))
	hash := hex.EncodeToString(hmacSHA256(secret, []byte(strings.Join(parts, "\n"))))

	q := url.Values{}
	for k, v := range fields {
		q.Set(k, v)
	}
	q.Set("hash", hash)
	return q.Encode()
}

func TestVerifyInitData(t *testing.T) {
	const token = "123456:test-bot-token"
	now := time.Unix(1_700_000_000, 0)
	baseFields := func() map[string]string {
		return map[string]string{
			"auth_date": strconv.FormatInt(now.Unix(), 10),
			"query_id":  "AAABBBCCC",
			"user":      `{"id":5504926420,"first_name":"Pol","username":"pol"}`,
		}
	}

	t.Run("valid", func(t *testing.T) {
		data := signInitData(token, baseFields())
		user, err := verifyInitData(data, token, time.Hour, now)
		require.NoError(t, err)
		assert.Equal(t, int64(5504926420), user.ID)
		assert.Equal(t, "Pol", user.FirstName)
	})

	t.Run("tampered value", func(t *testing.T) {
		data := signInitData(token, baseFields())
		// flip the user id after signing → signature no longer matches
		tampered := strings.Replace(data, "5504926420", "999", 1)
		_, err := verifyInitData(tampered, token, time.Hour, now)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "signature")
	})

	t.Run("wrong token", func(t *testing.T) {
		data := signInitData(token, baseFields())
		_, err := verifyInitData(data, "other-token", time.Hour, now)
		require.Error(t, err)
	})

	t.Run("empty token", func(t *testing.T) {
		_, err := verifyInitData("hash=abc", "", time.Hour, now)
		require.Error(t, err)
	})

	t.Run("no hash", func(t *testing.T) {
		_, err := verifyInitData("auth_date=1&user=%7B%7D", token, time.Hour, now)
		require.Error(t, err)
	})

	t.Run("expired", func(t *testing.T) {
		data := signInitData(token, baseFields())
		_, err := verifyInitData(data, token, time.Minute, now.Add(2*time.Hour))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "expired")
	})

	t.Run("maxAge zero skips freshness", func(t *testing.T) {
		data := signInitData(token, baseFields())
		_, err := verifyInitData(data, token, 0, now.Add(10_000*time.Hour))
		require.NoError(t, err)
	})
}

func TestMiniAuthMiddleware(t *testing.T) {
	const token = "123456:test-bot-token"
	srv := &Server{BotToken: token, AllowedUserID: 5504926420}
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	handler := srv.miniAuth(ok)

	valid := signInitData(token, map[string]string{
		"auth_date": strconv.FormatInt(time.Now().Unix(), 10),
		"user":      `{"id":5504926420,"first_name":"Pol"}`,
	})
	wrongUser := signInitData(token, map[string]string{
		"auth_date": strconv.FormatInt(time.Now().Unix(), 10),
		"user":      `{"id":42,"first_name":"Stranger"}`,
	})

	tests := []struct {
		name   string
		header string
		want   int
	}{
		{"no header", "", http.StatusUnauthorized},
		{"garbage", "not-init-data", http.StatusUnauthorized},
		{"wrong user", wrongUser, http.StatusUnauthorized},
		{"allowed user", valid, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/wegweiser/api/status", http.NoBody)
			if tt.header != "" {
				req.Header.Set(initDataHeader, tt.header)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			assert.Equal(t, tt.want, rec.Code)
		})
	}
}
