package main

import (
	"strings"
	"testing"
)

const good = `
name: Release
on:
  push:
    tags: ["v*"]
permissions:
  contents: read
jobs:
  build:
    runs-on: ubuntu-latest
    permissions:
      contents: read
    steps:
      - uses: actions/checkout@abc
      - uses: actions/setup-go@abc
        with:
          go-version: "1.27"
          cache: false
      - uses: actions/setup-node@abc
        with:
          node-version-file: web/.nvmrc
          package-manager-cache: false
      - run: npm ci
      - run: scripts/release/build-deb.sh "$TAG" amd64 dist
      - uses: actions/upload-artifact@abc
        with:
          name: debs
          path: out
  sign:
    needs: build
    runs-on: ubuntu-latest
    environment: release
    permissions:
      contents: write
    steps:
      - uses: actions/checkout@abc
        with:
          persist-credentials: false
      - uses: actions/download-artifact@abc
        with:
          name: debs
          path: downloaded
      - env:
          KEY: ${{ secrets.HOSERVA_RELEASE_SIGNING_KEY }}
        run: |
          # make nothing here
          scripts/release/stage-release-artifacts.sh "$TAG" downloaded staged
          scripts/release/publish-release.sh "$TAG" staged key
`

func TestCheck(t *testing.T) {
	cases := []struct {
		name, from, to, want string
	}{
		{"good", "", "", ""},
		{"npm in the signing job", "          # make nothing here\n", "          npm ci\n", "runs npm"},
		{"make in the signing job", "          # make nothing here\n", "          make web-build\n", "runs make"},
		{"go run in the signing job", "          # make nothing here\n", "          go run ./x\n", "runs go run"},
		{"go build in the signing job", "          # make nothing here\n", "          go build ./...\n", "runs go run"},
		{"dpkg-buildpackage in the signing job", "          # make nothing here\n", "          dpkg-buildpackage -b\n", "dpkg-buildpackage"},
		{"build-deb.sh in the signing job", "          # make nothing here\n", "          scripts/release/build-deb.sh v1 amd64 d\n", "build-deb.sh"},
		{"setup-node in the signing job", "      - uses: actions/download-artifact@abc", "      - uses: actions/setup-node@abc\n      - uses: actions/download-artifact@abc", "actions/setup-node"},
		{"setup-go in the signing job", "      - uses: actions/download-artifact@abc", "      - uses: actions/setup-go@abc\n      - uses: actions/download-artifact@abc", "actions/setup-go"},
		{"curl in the signing job", "          # make nothing here\n", "          curl -O https://x/a.deb\n", "downloaded artifact"},
		{"no artifact download", "      - uses: actions/download-artifact@abc\n        with:\n          name: debs\n          path: downloaded\n", "", "download-artifact"},
		{"signing job does not need the build", "    needs: build\n", "", "needs no other job"},
		{"signing job needs the wrong job", "    needs: build\n", "    needs: other\n", "unknown job"},
		{"no stage step", "scripts/release/stage-release-artifacts.sh", "cp", "stage-release-artifacts.sh"},
		{"stage reads another directory", "stage-release-artifacts.sh \"$TAG\" downloaded", "stage-release-artifacts.sh \"$TAG\" elsewhere", "stage-release-artifacts.sh"},
		{"no environment", "    environment: release\n", "", "release environment"},
		{"credentials persisted", "          persist-credentials: false\n", "          persist-credentials: true\n", "persist-credentials"},
		{"build job writes", "  build:\n    runs-on: ubuntu-latest\n    permissions:\n      contents: read\n", "  build:\n    runs-on: ubuntu-latest\n    permissions:\n      contents: write\n", "write permission"},
		{"build job without permissions", "  build:\n    runs-on: ubuntu-latest\n    permissions:\n      contents: read\n", "  build:\n    runs-on: ubuntu-latest\n", "no permissions"},
		{"build job with the signing key", "      - run: npm ci\n", "      - run: npm ci\n        env:\n          K: ${{ secrets.HOSERVA_RELEASE_SIGNING_KEY }}\n", "more than one job"},
		{"build job with another secret", "      - run: npm ci\n", "      - run: npm ci\n        env:\n          K: ${{ secrets.OTHER }}\n", "references a secret"},
		{"build job with the token", "      - run: npm ci\n", "      - run: npm ci\n        env:\n          K: ${{ github.token }}\n", "workflow token"},
		{"build job in an environment", "  build:\n    runs-on: ubuntu-latest\n", "  build:\n    runs-on: ubuntu-latest\n    environment: release\n", "runs in an environment"},
		{"build job uploads nothing", "      - uses: actions/upload-artifact@abc\n        with:\n          name: debs\n          path: out\n", "", "upload-artifact"},
		{"setup-go without cache: false", "          go-version: \"1.27\"\n          cache: false\n", "          go-version: \"1.27\"\n", "actions/setup-go without cache: false"},
		{"setup-go with cache enabled", "          cache: false\n", "          cache: true\n", "actions/setup-go without cache: false"},
		{"setup-go without any inputs", "        with:\n          go-version: \"1.27\"\n          cache: false\n", "", "actions/setup-go without cache: false"},
		{"setup-node without package-manager-cache: false", "          package-manager-cache: false\n", "", "actions/setup-node without package-manager-cache: false"},
		{"setup-node with package-manager-cache enabled", "package-manager-cache: false", "package-manager-cache: true", "actions/setup-node without package-manager-cache: false"},
		{"setup-node naming a package manager", "          package-manager-cache: false\n", "          package-manager-cache: false\n          cache: npm\n", "actions/setup-node and sets cache: npm"},
		{"workflow-level write", "permissions:\n  contents: read\njobs:", "permissions:\n  contents: write\njobs:", "workflow-level permissions"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc := good
			if c.from != "" {
				if !strings.Contains(doc, c.from) {
					t.Fatalf("fixture does not contain %q", c.from)
				}
				doc = strings.Replace(doc, c.from, c.to, 1)
			}
			got := strings.Join(check([]byte(doc)), "\n")
			if c.want == "" {
				if got != "" {
					t.Fatalf("want no problems, got:\n%s", got)
				}
				return
			}
			if !strings.Contains(got, c.want) {
				t.Fatalf("want a problem containing %q, got:\n%s", c.want, got)
			}
		})
	}
}

func TestCheckAppliesTheCacheRuleToEveryJob(t *testing.T) {
	doc := strings.Replace(good, "  sign:\n", "  other:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/setup-go@abc\n  sign:\n", 1)
	got := strings.Join(check([]byte(doc)), "\n")
	if !strings.Contains(got, "job other step 1 uses actions/setup-go without cache: false") {
		t.Fatalf("want the cache rule to cover a job other than the build job, got:\n%s", got)
	}
}

func TestCheckRejectsASingleJob(t *testing.T) {
	doc := `
jobs:
  publish:
    environment: release
    permissions:
      contents: write
    steps:
      - uses: actions/setup-node@abc
      - run: scripts/release/build-deb.sh "$TAG" amd64 dist
      - env:
          K: ${{ secrets.HOSERVA_RELEASE_SIGNING_KEY }}
        run: scripts/release/publish-release.sh "$TAG" dist key
`
	got := strings.Join(check([]byte(doc)), "\n")
	for _, want := range []string{"build-deb.sh", "actions/setup-node", "download-artifact", "needs no other job"} {
		if !strings.Contains(got, want) {
			t.Errorf("want a problem containing %q, got:\n%s", want, got)
		}
	}
}
