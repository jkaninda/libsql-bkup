package libsql

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/jkaninda/bkupkit/config"
)

// endpoint is a resolved server address.
type endpoint struct {
	URL   string // http(s)://host[:port][/path], no query
	Token string
}

// resolveEndpoint builds the server URL from DB_URL, or from DB_HOST and
// DB_PORT, and picks the auth token (LIBSQL_AUTH_TOKEN, then a token
// embedded in DB_URL as ?authToken=, ?auth_token= or ?jwt=).
//
// Accepted schemes: http, https, libsql (https, or http with ?tls=0),
// ws and wss (mapped to http and https: sqld serves Hrana over HTTP on the
// same port as WebSockets).
func resolveEndpoint(v *config.Values) (endpoint, error) {
	raw := strings.TrimSpace(v.String(config.DBURL))
	if raw == "" {
		host := strings.TrimSpace(v.String(config.DBHost))
		if host == "" {
			host = "localhost"
		}
		if strings.Contains(host, "://") {
			raw = host
		} else {
			port := v.String(config.DBPort)
			if port == "" {
				port = "8080"
			}
			raw = "http://" + net.JoinHostPort(strings.Trim(host, "[]"), port)
		}
	}
	return parseURL(raw, v.String(AuthToken))
}

func parseURL(raw, token string) (endpoint, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return endpoint{}, fmt.Errorf("DB_URL: %w", redact(err))
	}
	if u.Host == "" {
		return endpoint{}, errors.New("DB_URL: missing host (expected e.g. http://localhost:8080 or libsql://db.example.com)")
	}
	q := u.Query()
	var urlToken string
	for _, k := range []string{"authToken", "auth_token", "jwt"} {
		if t := q.Get(k); t != "" {
			if urlToken != "" && urlToken != t {
				return endpoint{}, errors.New("DB_URL: more than one of authToken, auth_token and jwt")
			}
			urlToken = t
		}
		q.Del(k)
	}
	tls := ""
	if q.Has("tls") {
		tls = q.Get("tls")
		q.Del("tls")
		if tls != "0" && tls != "1" {
			return endpoint{}, fmt.Errorf("DB_URL: tls must be 0 or 1, got %q", tls)
		}
	}
	for k := range q {
		return endpoint{}, fmt.Errorf("DB_URL: unknown query parameter %q", k)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "ws":
		if tls == "1" {
			return endpoint{}, fmt.Errorf("DB_URL: %s:// cannot use tls=1", u.Scheme)
		}
		u.Scheme = "http"
	case "https", "wss":
		if tls == "0" {
			return endpoint{}, fmt.Errorf("DB_URL: %s:// cannot use tls=0", u.Scheme)
		}
		u.Scheme = "https"
	case "libsql":
		if tls == "0" {
			if u.Port() == "" {
				return endpoint{}, errors.New("DB_URL: libsql:// with tls=0 needs an explicit port")
			}
			u.Scheme = "http"
		} else {
			u.Scheme = "https"
		}
	default:
		return endpoint{}, fmt.Errorf("DB_URL: unsupported scheme %q (use http, https, libsql, ws or wss)", u.Scheme)
	}
	u.RawQuery = ""
	u.Fragment = ""
	u.User = nil
	if token == "" {
		token = urlToken
	}
	return endpoint{URL: strings.TrimRight(u.String(), "/"), Token: token}, nil
}

// redact drops the URL text (which may hold a token) from a parse error.
func redact(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// targetName is the name used for artifacts: DB_NAME, else the first label
// of the server host ("mydb" for mydb-org.turso.io), else "libsql".
func targetName(v *config.Values, ep endpoint) string {
	if n := strings.TrimSpace(v.String(config.DBName)); n != "" {
		return n
	}
	if u, err := url.Parse(ep.URL); err == nil {
		h := u.Hostname()
		if net.ParseIP(h) == nil && h != "localhost" && h != "" {
			if i := strings.IndexByte(h, '.'); i > 0 {
				h = h[:i]
			}
			return h
		}
	}
	return "libsql"
}
