package auth

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHTTPAuthenticator(t *testing.T) {
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req httpAuthRequest
		if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&req) != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		resp := httpAuthResponse{}
		if req.Addr == "123.123.123.123:5566" && req.Auth == "wahaha" && req.Tx == 12345 {
			resp.OK = true
			resp.ID = "some_unique_id"
		}
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(resp))
	}))
	defer testServer.Close()

	auth := NewHTTPAuthenticator(testServer.URL+"/auth", false)

	ok, id := auth.Authenticate(&net.UDPAddr{
		IP:   net.ParseIP("1.2.3.4"),
		Port: 34567,
	}, "idk", 123)
	assert.False(t, ok)
	assert.Equal(t, "", id)

	ok, id = auth.Authenticate(&net.UDPAddr{
		IP:   net.ParseIP("123.123.123.123"),
		Port: 5566,
	}, "wahaha", 12345)
	assert.True(t, ok)
	assert.Equal(t, "some_unique_id", id)
}
