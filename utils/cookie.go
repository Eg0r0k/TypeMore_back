package utils

import (
	"net/http"
	"time"
)

func  ClearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		Expires:  time.Now().Add(-1 * time.Hour),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
}

func SetCookie(w http.ResponseWriter, name, value string, path string, ttl time.Duration, httpOnly, secure bool, sameSite http.SameSite) {
	http.SetCookie(w, &http.Cookie{
		Name: name,
		Value: value,
		Path: path, 
		Expires:  time.Now().Add(ttl),
		HttpOnly: httpOnly,
		Secure:   secure,
		SameSite: sameSite,	
	})
}