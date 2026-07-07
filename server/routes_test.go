package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestResolveHomeLocale(t *testing.T) {
	cases := []struct {
		accept string
		want   HomeLocale
	}{
		{"ko-KR,ko;q=0.9,en-US;q=0.8", localeKO},
		{"zh-TW", localeZHHant},
		{"zh-Hant-HK", localeZHHant},
		{"zh-HK", localeZHHant},
		{"zh-MO", localeZHHant},
		{"zh-CN", localeZHHans},
		{"zh-Hans-SG", localeZHHans},
		{"zh", localeZHHans},
		{"pt-BR,pt;q=0.9", localePT},
		{"fr-FR,fr;q=0.9", localeFR},
		{"es-419", localeES},
		{"de-DE,it;q=0.8", localeEN},
		{"", localeEN},
	}
	for _, tc := range cases {
		if got := resolveHomeLocale(tc.accept); got != tc.want {
			t.Errorf("resolveHomeLocale(%q) = %q, want %q", tc.accept, got, tc.want)
		}
	}
	if loc, ok := asHomeLocale("zh-hant"); !ok || loc != localeZHHant {
		t.Errorf("asHomeLocale(zh-hant) = %q, %v; want zh-hant", loc, ok)
	}
	if _, ok := asHomeLocale("nope"); ok {
		t.Error("asHomeLocale(nope) should not match")
	}
}

// Every supported locale must have a full string table: homeStrings literals
// use named fields, so a forgotten field silently renders as "".
func TestHomeLocalesHaveStrings(t *testing.T) {
	for _, l := range homeLocales {
		s, ok := homeStringsByLocale[l]
		if !ok {
			t.Errorf("locale %q missing from homeStringsByLocale", l)
			continue
		}
		v := reflect.ValueOf(s)
		for i := 0; i < v.NumField(); i++ {
			field := v.Field(i)
			name := v.Type().Field(i).Name
			switch field.Kind() {
			case reflect.String:
				if field.String() == "" {
					t.Errorf("locale %q: %s is empty", l, name)
				}
			case reflect.Struct:
				for j := 0; j < field.NumField(); j++ {
					if field.Field(j).String() == "" {
						t.Errorf("locale %q: %s.%s is empty", l, name, field.Type().Field(j).Name)
					}
				}
			}
		}
		// The web app rewrites the literal "UTC" to the browser time zone
		// (chartTimeLabel); every translation must keep that token.
		if !strings.Contains(s.JS.TimeUTC, "UTC") {
			t.Errorf("locale %q TimeUTC = %q must contain the literal \"UTC\"", l, s.JS.TimeUTC)
		}
	}
}
