package web

import "testing"

func TestDetectLocaleFromHeaders(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   string
	}{
		{name: "zh cn", header: "zh-CN,zh;q=0.9,en;q=0.8", want: localeZH},
		{name: "zh tw", header: "zh-TW,zh;q=0.8,en-US;q=0.6", want: localeZH},
		{name: "english", header: "en-US,en;q=0.9", want: localeEN},
		{name: "empty", header: "", want: localeEN},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := detectLocaleFromHeaders(tt.header); got != tt.want {
				t.Fatalf("detectLocaleFromHeaders(%q) = %q, want %q", tt.header, got, tt.want)
			}
		})
	}
}

func TestTranslationsHaveMatchingLocaleKeys(t *testing.T) {
	base := translations[localeEN]
	target := translations[localeZH]
	if len(base) == 0 || len(target) == 0 {
		t.Fatal("translations must define both supported locales")
	}

	for key := range base {
		if _, ok := target[key]; !ok {
			t.Fatalf("%s is missing translation key %q", localeZH, key)
		}
	}
	for key := range target {
		if _, ok := base[key]; !ok {
			t.Fatalf("%s is missing translation key %q", localeEN, key)
		}
	}
}
