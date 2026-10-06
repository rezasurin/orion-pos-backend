// Package archtest enforces the module boundaries from docs/BACKEND_PLAN.md section 3, which the
// compiler cannot: a business module may use another module only through that module's root
// package (its Service and types), never its generated queries or other internals.
package archtest

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"
)

const modulePath = "github.com/rezasurin/orion-pos-backend"

// composition is the one package under internal/ that may import every module: the HTTP layer
// that implements the OpenAPI operations by calling them. Only cmd/ imports it.
const composition = modulePath + "/internal/api"

// Business modules from the plan. Packages under internal/ that are not listed here (kernel,
// config, database, httpserver, testdb) are shared infrastructure.
var businessModules = map[string]bool{
	"platform": true, "tenancy": true, "identity": true, "entitlements": true, "signup": true,
	"catalog": true, "sales": true, "payments": true, "inventory": true, "sync": true,
	"reporting": true, "billing": true, "notify": true,
}

type pkg struct {
	ImportPath   string
	Imports      []string
	TestImports  []string
	XTestImports []string
}

func TestModuleBoundaries(t *testing.T) {
	cmd := exec.Command("go", "list", "-json", modulePath+"/...")
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			t.Fatalf("go list: %v\n%s", err, exitErr.Stderr)
		}
		t.Fatalf("go list: %v", err)
	}

	dec := json.NewDecoder(bytes.NewReader(out))
	checked := 0
	for {
		var p pkg
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		checked++
		from := moduleOf(p.ImportPath)
		for _, imp := range p.Imports {
			if msg := violation(p.ImportPath, from, imp, false); msg != "" {
				t.Error(msg)
			}
		}
		for _, imp := range append(append([]string{}, p.TestImports...), p.XTestImports...) {
			if msg := violation(p.ImportPath, from, imp, true); msg != "" {
				t.Error(msg)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no packages listed")
	}
}

// violation says why importer (in module from, or "" for shared packages) may not import imp, or
// returns "". fromTest is true for imports made by test files.
func violation(importer, from, imp string, fromTest bool) string {
	if imp == composition && importer != composition && !strings.HasPrefix(importer, modulePath+"/cmd/") && !strings.HasPrefix(importer, composition+"/") {
		return importer + " imports " + imp + ": only cmd/ may import the composition package"
	}
	to := moduleOf(imp)
	if to == "" || to == from {
		return ""
	}
	// A module's test support, <module>/<module>stub (for example sync/syncstub, a stand-in
	// projector), may be used by other packages' tests but never by production code.
	if fromTest && imp == modulePath+"/internal/"+to+"/"+to+"stub" {
		return ""
	}
	// Another module's root package is its public API.
	if imp == modulePath+"/internal/"+to {
		// Infrastructure must not depend on business modules; only cmd/ wires them together.
		if from == "" && strings.HasPrefix(importer, modulePath+"/internal/") && importer != composition {
			return importer + " is infrastructure and must not import module " + to
		}
		return ""
	}
	return importer + " imports " + imp + ": use the " + to + " module's root package instead"
}

// moduleOf returns the business module a package belongs to, or "" for anything else.
func moduleOf(importPath string) string {
	rest, ok := strings.CutPrefix(importPath, modulePath+"/internal/")
	if !ok {
		return ""
	}
	name, _, _ := strings.Cut(rest, "/")
	if businessModules[name] {
		return name
	}
	return ""
}

func TestViolation(t *testing.T) {
	m := modulePath + "/internal/"
	tests := []struct {
		importer, imp string
		bad           bool
		fromTest      bool
	}{
		{m + "api", m + "sync/syncstub", false, true},
		{m + "api", m + "sync/syncstub", true, false},
		{m + "api", m + "sync/projector", true, true},
		{m + "api", m + "tenancy/syncstub", true, true},
		{m + "sales", m + "tenancy", false, false},
		{m + "sales", m + "tenancy/db", true, false},
		{m + "sales/projector", m + "inventory/ledger", true, false},
		{m + "tenancy", m + "tenancy/db", false, false},
		{m + "tenancy", m + "kernel", false, false},
		{m + "httpserver", m + "tenancy", true, false},
		{modulePath + "/cmd/orion", m + "tenancy", false, false},
		{modulePath + "/cmd/orion", m + "tenancy/db", true, false},
		{m + "api", m + "identity", false, false},
		{m + "api", m + "identity/db", true, false},
		{modulePath + "/cmd/orion", m + "api", false, false},
		{m + "identity", m + "api", true, false},
		{m + "httpserver", m + "api", true, false},
	}
	for _, tt := range tests {
		got := violation(tt.importer, moduleOf(tt.importer), tt.imp, tt.fromTest) != ""
		if got != tt.bad {
			t.Errorf("violation(%s -> %s) = %v, want %v", tt.importer, tt.imp, got, tt.bad)
		}
	}
}
