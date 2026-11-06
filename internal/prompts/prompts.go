// Package prompts provides embedded prompt templates for the finder and
// verifier stages. Templates are embedded at build time; callers may
// supply override file paths to substitute their own texts.
package prompts

import (
	"bytes"
	_ "embed"
	"fmt"
	"os"
	"regexp"
	"strings"
	"text/template"
)

//go:embed templates/finder_core.tmpl
var finderCoreTmpl string

//go:embed templates/finder_scope_diff.tmpl
var finderScopeDiffTmpl string

//go:embed templates/finder_scope_survey.tmpl
var finderScopeSurveyTmpl string

//go:embed templates/finder_prose.tmpl
var finderProseTmpl string

//go:embed templates/finder_mixed_addendum.tmpl
var finderMixedAddendumTmpl string

//go:embed templates/finder_quick_addendum.tmpl
var finderQuickAddendumTmpl string

//go:embed templates/finder_go_addendum.tmpl
var finderGoAddendumTmpl string

//go:embed templates/finder_frontend_addendum.tmpl
var finderFrontendAddendumTmpl string

//go:embed templates/finder_react_hooks_addendum.tmpl
var finderReactHooksAddendumTmpl string

//go:embed templates/finder_user.tmpl
var finderUserTmpl string

//go:embed templates/verifier_system.tmpl
var verifierSystemTmpl string

//go:embed templates/verifier_user.tmpl
var verifierUserTmpl string

// FinderUserVars holds the template variables for the finder user message.
type FinderUserVars struct {
	Task        string
	Diff        string
	FileContext string
	Survey      bool
	Manifest    string
}

// VerifierVars holds the template variables for the verifier user message.
// Deliberately does NOT include Reasoning — anti-anchoring: the verifier
// must not see the finder's chain of thought.
type VerifierVars struct {
	Severity    string
	Confidence  float64
	File        string
	Lines       string
	Description string
	CodeQuote   string
	FixedCode   string
	FileSnippet string
}

// Finder returns the finder system prompt for the given mode, scope, quick
// flag, and detected language families.
//
// mode must be "code", "prose", or "mixed". The code/mixed prompt is assembled as
// finder_core + scope framing + lang addenda (+ mixed addendum for mixed),
// with the quick-mode constraints addendum appended last so it overrides
// everything above it. langs selects language-scoped addenda (e.g. "go",
// "react", "frontend"); see appendLangAddenda.
//
// overridePath, when non-empty, replaces only the CORE template — the
// scope framing stays roast-controlled because it encodes review invariants,
// not taste. Prose mode ignores scope and uses finder_prose.tmpl as its
// whole base; overridePath there replaces that base wholesale.
func Finder(mode, scope string, quick bool, langs []string, overridePath string) (string, error) {
	var base string
	switch mode {
	case "prose":
		var err error
		base, err = load(overridePath, func() (string, error) { return finderProseTmpl, nil })
		if err != nil {
			return "", fmt.Errorf("finder prose prompt: %w", err)
		}
		if scope == "repo" {
			base += scopeFraming(scope)
		}
	case "mixed":
		var err error
		base, err = load(overridePath, func() (string, error) { return finderCoreTmpl, nil })
		if err != nil {
			return "", fmt.Errorf("finder core prompt: %w", err)
		}
		base += scopeFraming(scope)
		base += finderMixedAddendumTmpl
		base += appendLangAddenda(langs)
	default: // "code" and anything unknown
		var err error
		base, err = load(overridePath, func() (string, error) { return finderCoreTmpl, nil })
		if err != nil {
			return "", fmt.Errorf("finder core prompt: %w", err)
		}
		base += scopeFraming(scope)
		base += appendLangAddenda(langs)
	}
	if quick {
		base += finderQuickAddendumTmpl
	}
	return base, nil
}

// scopeFraming returns the scope-specific framing appended after the core
// template. "diff", "" (unset), and any not-yet-recognized scope select the
// diff-mode framing; a survey scope will branch here in a later phase.
func scopeFraming(scope string) string {
	if scope == "repo" {
		return finderScopeSurveyTmpl
	}
	return finderScopeDiffTmpl
}

// appendLangAddenda returns the concatenated language-scoped addenda
// selected by langs, in a stable order. "go" appends the Go addendum.
// "react" appends the frontend addendum plus the React-hooks addendum;
// "frontend" appends the frontend addendum alone (no hooks guidance — Vue/
// Svelte/Astro have their own reactivity models). The frontend addendum is
// appended at most once even when both "react" and "frontend" are present.
// "web" is accepted but triggers no addendum in v1.
func appendLangAddenda(langs []string) string {
	var has = make(map[string]bool, len(langs))
	for _, l := range langs {
		has[l] = true
	}
	var out string
	if has["go"] {
		out += finderGoAddendumTmpl
	}
	if has["react"] || has["frontend"] {
		out += finderFrontendAddendumTmpl
	}
	if has["react"] {
		out += finderReactHooksAddendumTmpl
	}
	return out
}

// FinderUser renders the finder user message template.
func FinderUser(vars FinderUserVars) string {
	return mustRender(finderUserTmpl, vars)
}

// Verifier returns the verifier system prompt. overridePath, when non-empty,
// replaces the embedded default.
func Verifier(overridePath string) (string, error) {
	return load(overridePath, func() (string, error) { return verifierSystemTmpl, nil })
}

// VerifierUser renders the verifier user message template.
func VerifierUser(vars VerifierVars) string {
	return mustRender(verifierUserTmpl, vars)
}

// load returns text from overridePath when non-empty, falling back to the
// result of fallback(). A non-empty path that doesn't exist is an error.
func load(overridePath string, fallback func() (string, error)) (string, error) {
	if overridePath == "" {
		return fallback()
	}
	data, err := os.ReadFile(overridePath)
	if err != nil {
		return "", fmt.Errorf("prompt override %s: %w", overridePath, err)
	}
	return string(data), nil
}

// verifierDataClose matches a closing tag of any wrapper verifier_user.tmpl
// puts around finder- or repository-supplied text.
var verifierDataClose = regexp.MustCompile(`(?i)</\s*(claim|code_quote|fixed_code|file_contents)\b`)

// dataText is the `data` template function. It defangs any closing wrapper
// tag inside untrusted text, so a claim or file cannot end its own tag early
// and append text that reads as part of the prompt.
func dataText(s string) string {
	return verifierDataClose.ReplaceAllString(s, `<\/$1`)
}

// mustRender renders a text/template with data. Panics on parse error
// (templates are compile-time constants; parse failure is a bug).
func mustRender(tmplText string, data any) string {
	t := template.Must(template.New("").Funcs(template.FuncMap{"data": dataText}).Parse(tmplText))
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		panic(fmt.Sprintf("prompts: template execution failed: %v", err))
	}
	return buf.String()
}

// TrimSpace returns the prompt with leading/trailing whitespace removed.
// Useful when callers want a clean string for length checks.
func TrimSpace(s string) string {
	return strings.TrimSpace(s)
}
