package template

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

const keyProbe = "services:\n  probe:\n    image: x\n    environment:\n      APP_KEY: ${APP_KEY}\nx-hoserva:\n  schema: 1\n  id: probe\n  revision: 1\n  title: Probe\n  categories: [system]\n  icon: icon.svg\n  docs: https://example.com\n  inputs:\n    APP_KEY: { kind: secret%s }\n"

func keyInstaller(t *testing.T, format string) (*Installer, *fakeStacks) {
	t.Helper()
	in, stacks := newInstaller(t)
	in.Random = nil
	in.Catalog = MapCatalog{Templates: map[string]string{"probe": strings.Replace(keyProbe, "%s", format, 1)}}
	return in, stacks
}

func decodeLaravelKey(t *testing.T, v string) []byte {
	t.Helper()
	rest, ok := strings.CutPrefix(v, "base64:")
	if !ok {
		t.Fatalf("%q has no base64: prefix", v)
	}
	raw, err := base64.StdEncoding.DecodeString(rest)
	if err != nil {
		t.Fatalf("%q: %v", v, err)
	}
	return raw
}

func TestALaravelKeySecretIsGeneratedAsBase64Of32RandomBytes(t *testing.T) {
	in, stacks := keyInstaller(t, ", format: laravel-key")
	for _, name := range []string{"one", "two"} {
		if _, _, err := in.Install(context.Background(), PlanRequest{ID: "probe", Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	a, b := envLines(stacks.created[0].Env)["APP_KEY"], envLines(stacks.created[1].Env)["APP_KEY"]
	if len(decodeLaravelKey(t, a)) != 32 || a == b {
		t.Errorf("keys %q and %q must each hold 32 bytes and differ", a, b)
	}
}

func TestASecretWithoutAFormatStaysHexAndFormatHexIsTheSame(t *testing.T) {
	for _, format := range []string{"", ", format: hex"} {
		in, stacks := keyInstaller(t, format)
		in.Random = zeroReader{}
		if _, _, err := in.Install(context.Background(), PlanRequest{ID: "probe"}); err != nil {
			t.Fatal(err)
		}
		if got, want := envLines(stacks.created[0].Env)["APP_KEY"], strings.Repeat("ab", 24); got != want {
			t.Errorf("format %q: APP_KEY = %q, want %q", format, got, want)
		}
	}
}

func TestATypedLaravelKeyMustDecodeTo32Bytes(t *testing.T) {
	good := "base64:" + base64.StdEncoding.EncodeToString(make([]byte, 32))
	short := "base64:" + base64.StdEncoding.EncodeToString(make([]byte, 16))
	cases := []struct {
		name, value string
		ok          bool
	}{
		{"valid", good, true},
		{"too short", short, false},
		{"no prefix", strings.TrimPrefix(good, "base64:"), false},
		{"not base64", "base64:***", false},
		{"free text", "hunter2", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in, stacks := keyInstaller(t, ", format: laravel-key")
			_, _, err := in.Install(context.Background(), PlanRequest{ID: "probe", Values: map[string]string{"APP_KEY": tc.value}})
			if tc.ok {
				if err != nil {
					t.Fatal(err)
				}
				if got := envLines(stacks.created[0].Env)["APP_KEY"]; got != tc.value {
					t.Errorf("APP_KEY = %q, want the typed %q", got, tc.value)
				}
				return
			}
			var ie *InputError
			if !errors.As(err, &ie) || ie.Input != "APP_KEY" || !errors.Is(err, ErrInvalidInput) || len(stacks.created) != 0 {
				t.Fatalf("err = %v, created = %d, want an input error naming APP_KEY and no stack", err, len(stacks.created))
			}
		})
	}
}

func TestATypedValueOfAFormatlessSecretIsKeptWhatItLooksLike(t *testing.T) {
	in, stacks := keyInstaller(t, "")
	if _, _, err := in.Install(context.Background(), PlanRequest{ID: "probe", Values: map[string]string{"APP_KEY": "hunter2"}}); err != nil {
		t.Fatal(err)
	}
	if got := envLines(stacks.created[0].Env)["APP_KEY"]; got != "hunter2" {
		t.Errorf("APP_KEY = %q", got)
	}
}

func TestAnUpdateOfALaravelKeyChecksAndRegeneratesByFormat(t *testing.T) {
	in, stacks := keyInstaller(t, ", format: laravel-key")
	if _, _, err := in.Install(context.Background(), PlanRequest{ID: "probe"}); err != nil {
		t.Fatal(err)
	}
	before := stacks.created[0].Env
	if _, err := in.UpdateConfig(context.Background(), "probe", ConfigUpdate{Values: map[string]string{"APP_KEY": "base64:AAAA"}}); !errors.Is(err, ErrInvalidInput) || stacks.created[0].Env != before {
		t.Fatalf("a short key: err = %v, env changed = %v", err, stacks.created[0].Env != before)
	}
	if _, err := in.UpdateConfig(context.Background(), "probe", ConfigUpdate{Generate: []string{"APP_KEY"}}); err != nil {
		t.Fatal(err)
	}
	if got := envLines(stacks.created[0].Env)["APP_KEY"]; len(decodeLaravelKey(t, got)) != 32 {
		t.Errorf("a regenerated APP_KEY = %q", got)
	}
}

func TestAPreviewSummaryUsesAPlaceholderOfTheFormat(t *testing.T) {
	if got := decodeLaravelKey(t, secretPlaceholder(FormatLaravelKey)); len(got) != 32 {
		t.Errorf("laravel-key placeholder holds %d bytes", len(got))
	}
	if got := secretPlaceholder(""); got != strings.Repeat("0", 48) {
		t.Errorf("hex placeholder = %q", got)
	}
}

func TestLintChecksTheFormatOfASecretInput(t *testing.T) {
	cases := []struct {
		name, input, want string
	}{
		{"laravel-key secret", "{ kind: secret, format: laravel-key }", ""},
		{"hex secret", "{ kind: secret, format: hex }", ""},
		{"unknown format", "{ kind: secret, format: base58 }", "x-hoserva.inputs.APP_KEY.format"},
		{"format on a string", "{ kind: string, format: hex }", "x-hoserva.inputs.APP_KEY"},
		{"format on a timezone", "{ kind: timezone, format: laravel-key }", "x-hoserva.inputs.APP_KEY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := strings.Replace(keyProbe, "{ kind: secret%s }", tc.input, 1)
			got := strings.Join(lintStrings(t, catalogFrom(t, "probe", src)), "\n")
			if tc.want == "" {
				if got != "" {
					t.Fatalf("lint = %s, want clean", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("findings do not mention %q:\n%s", tc.want, got)
			}
		})
	}
}
