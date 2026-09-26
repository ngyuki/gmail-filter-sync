package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"strings"
	"testing"
)

func rule(t *testing.T) Filter {
	t.Helper()
	f, e := normalize(map[string]any{"criteria": map[string]any{"from": "news@example.com"}, "action": map[string]any{"removeLabelIds": []any{"INBOX"}}})
	if e != nil {
		t.Fatal(e)
	}
	return f
}

func raw(f Filter) any {
	b, _ := json.Marshal(f)
	var v any
	json.Unmarshal(b, &v)
	return v
}

func save(t *testing.T, path, content string) {
	t.Helper()
	if e := os.WriteFile(path, []byte(content), 0600); e != nil {
		t.Fatal(e)
	}
}

func TestJsonnet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "filter.jsonnet")
	save(t, path, "// comment\n{ filters: [{ criteria: { from: 'news@example.com' }, action: { removeLabelIds: ['INBOX'] } }] }\n")
	fs, e := load(path)
	if e != nil || len(fs) != 1 || key(fs[0]) != key(rule(t)) {
		t.Fatalf("load: %v %v", fs, e)
	}
	save(t, path, "{ filters: [] } { filters: [] }")
	if fs, e = load(path); e != nil || len(fs) != 0 {
		t.Fatalf("load valid Jsonnet object extension: %v %v", fs, e)
	}
	for _, bad := range []string{"", "null", "{ filters: null }", "{", "{ filters: [], extra: true }", "{ filters: [{}] }", "{ filters: [{ action: { forward: 123 } }] }", "{ filters: [{ criteria: { size: true }, action: { forward: 'a' } }] }", "{ filters: [{ criteria: { size: 100 }, action: { forward: 'a' } }] }", "{ filters: [{ action: { addLabelIds: ['X'], removeLabelIds: ['X'] } }] }"} {
		save(t, path, bad)
		if _, e := load(path); e == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestNormalizeAndDiff(t *testing.T) {
	t.Setenv("GMAIL_FILTER_SYNC_DIFF_FILTER", "")
	a, e := normalize(map[string]any{"criteria": map[string]any{"hasAttachment": false}, "action": map[string]any{"addLabelIds": []any{"B", "A", "A"}}})
	if e != nil {
		t.Fatal(e)
	}
	b, e := normalize(map[string]any{"criteria": map[string]any{}, "action": map[string]any{"addLabelIds": []any{"A", "B"}}})
	if e != nil || key(a) != key(b) {
		t.Fatal("normalization mismatch", e)
	}
	a.ID = "first"
	b.ID = "second"
	c := diff([]Filter{a}, []Filter{a, b})
	if len(c.Create) != 0 || len(c.Delete) != 1 || c.Delete[0].ID != "second" {
		t.Fatal(c)
	}
	var out bytes.Buffer
	if err := printChanges(&out, c); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "--- current\n+++ desired\n@@ -1,") || strings.Contains(out.String(), `"id"`) {
		t.Fatalf("unexpected unified diff:\n%s", out.String())
	}
	old := Filter{Criteria: map[string]any{"query": "from:old@example.com"}, Action: map[string]any{"addLabelIds": []string{"INBOX"}}}
	newRule := Filter{Criteria: map[string]any{"query": "from:new@example.com"}, Action: map[string]any{"addLabelIds": []string{"INBOX"}}}
	changeOutput := new(bytes.Buffer)
	if err := printChanges(changeOutput, Changes{Create: []Filter{newRule}, Delete: []Filter{old}}); err != nil {
		t.Fatal(err)
	}
	change := changeOutput.String()
	if !strings.Contains(change, `-    "query": "from:old@example.com"`) || !strings.Contains(change, `+    "query": "from:new@example.com"`) || strings.Contains(change, `-  "action"`) || strings.Contains(change, `+  "action"`) {
		t.Fatalf("unchanged JSON lines were marked as changes:\n%s", change)
	}
	t.Setenv("GMAIL_FILTER_SYNC_DIFF_FILTER", "sed 's/^-/-REMOVED:/'")
	out.Reset()
	if err := printChanges(&out, c); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "-REMOVED:") {
		t.Fatalf("diff filter was not applied:\n%s", out.String())
	}
}

func TestWriteAndDuplicate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "filter.jsonnet")
	f := rule(t)
	if e := writeJsonnet(path, document([]Filter{f})); e != nil {
		t.Fatal(e)
	}
	if e := writeJsonnet(path, Document{}); e == nil {
		t.Fatal("overwrote file")
	}
	if e := writeJsonnet(path, Document{Filters: []Filter{}}, true); e != nil {
		t.Fatal("force did not overwrite file:", e)
	}
	replaced, e := os.ReadFile(path)
	if e != nil || !strings.Contains(string(replaced), `"filters": []`) {
		t.Fatalf("force output mismatch: %s, %v", replaced, e)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode())
	}
	path = filepath.Join(t.TempDir(), "duplicate.jsonnet")
	writeJsonnet(path, document([]Filter{f, f}))
	if filters, e := load(path); e != nil || len(filters) != 2 {
		t.Fatalf("could not load duplicate filters: %v, %v", filters, e)
	}
}

func TestImportPreservesDuplicateFilters(t *testing.T) {
	t.Setenv("GMAIL_FILTER_SYNC_DIFF_FILTER", "")
	path := filepath.Join(t.TempDir(), "filters.jsonnet")
	api := func(method, user, id string, body *Filter) (map[string]any, error) {
		if method != "list" {
			t.Fatalf("unexpected API call: %s", method)
		}
		return map[string]any{"filter": []any{
			map[string]any{"id": "first", "criteria": map[string]any{"from": "news@example.com"}, "action": map[string]any{"forward": "dest@example.com"}},
			map[string]any{"id": "second", "criteria": map[string]any{"from": "news@example.com"}, "action": map[string]any{"forward": "dest@example.com"}},
		}}, nil
	}
	if err := run([]string{"--file", path, "import"}, api, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	filters, err := load(path)
	if err != nil || len(filters) != 2 {
		t.Fatalf("imported duplicate filters: %v, %v", filters, err)
	}
	var out bytes.Buffer
	if err := run([]string{"--file", path, "diff"}, api, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("unchanged import produced a diff: %s", out.String())
	}
	if err := run([]string{"--file", path, "apply"}, api, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestApply(t *testing.T) {
	t.Setenv("GMAIL_FILTER_SYNC_DIFF_FILTER", "")
	old := rule(t)
	old.ID = "old"
	desired := rule(t)
	desired.Criteria = map[string]any{"subject": "new"}
	state := []Filter{old}
	calls := []string{}
	api := func(method, user, id string, body *Filter) (map[string]any, error) {
		calls = append(calls, method)
		switch method {
		case "create":
			f := *body
			f.ID = "new"
			state = append(state, f)
			return map[string]any{"id": "new"}, nil
		case "delete":
			state = state[1:]
			return map[string]any{}, nil
		default:
			items := []any{}
			for _, f := range state {
				items = append(items, raw(f))
			}
			return map[string]any{"filter": items}, nil
		}
	}
	if e := apply([]Filter{desired}, state, "me", false, api, io.Discard, io.Discard); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(calls, []string{"create", "delete", "list"}) {
		t.Fatal(calls)
	}
	if e := apply([]Filter{desired}, state, "me", false, api, io.Discard, io.Discard); e != nil || len(calls) != 3 {
		t.Fatal("not idempotent", e)
	}
}

func TestApplyGuardsAndFailure(t *testing.T) {
	t.Setenv("GMAIL_FILTER_SYNC_DIFF_FILTER", "")
	old := rule(t)
	old.ID = "old"
	calls := []string{}
	fail := func(method, user, id string, f *Filter) (map[string]any, error) {
		calls = append(calls, method)
		return nil, errors.New("failure")
	}
	if e := apply(nil, []Filter{old}, "me", false, fail, io.Discard, io.Discard); e == nil || len(calls) != 0 {
		t.Fatal("empty guard")
	}
	desired := rule(t)
	desired.Criteria = map[string]any{"subject": "new"}
	if e := apply([]Filter{desired}, []Filter{old}, "me", false, fail, io.Discard, io.Discard); e == nil || !reflect.DeepEqual(calls, []string{"create"}) {
		t.Fatal("failure guard", e, calls)
	}
	if e := apply([]Filter{desired}, make([]Filter, 1000), "me", false, fail, io.Discard, io.Discard); e == nil {
		t.Fatal("limit guard")
	}
	calls = nil
	empty := func(method, user, id string, f *Filter) (map[string]any, error) {
		calls = append(calls, method)
		return map[string]any{}, nil
	}
	if e := apply(nil, []Filter{old}, "me", true, empty, io.Discard, io.Discard); e != nil || !reflect.DeepEqual(calls, []string{"delete", "list"}) {
		t.Fatal(e, calls)
	}
}

func TestCurrentInvalid(t *testing.T) {
	for _, data := range []map[string]any{{"filter": nil}, {"unexpected": true}, {"filter": []any{raw(rule(t))}}} {
		_, e := current("me", func(string, string, string, *Filter) (map[string]any, error) { return data, nil })
		if e == nil {
			t.Fatal("accepted invalid response")
		}
	}
}

func TestVersion(t *testing.T) {
	originalVersion := version
	version = "v1.2.3-test"
	t.Cleanup(func() { version = originalVersion })
	var out bytes.Buffer
	api := func(string, string, string, *Filter) (map[string]any, error) {
		t.Fatal("version must not call the API")
		return nil, nil
	}
	if err := run([]string{"--version"}, api, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	info, _ := debug.ReadBuildInfo()
	want := "gmail-filter-sync " + resolveVersion(version, info) + "\n"
	if out.String() != want {
		t.Fatalf("version output: got %q, want %q", out.String(), want)
	}
	if err := run([]string{"--version=value"}, api, io.Discard, io.Discard); err == nil {
		t.Fatal("accepted a value for --version")
	}
}

func TestResolveVersion(t *testing.T) {
	for _, tc := range []struct {
		name     string
		embedded string
		info     *debug.BuildInfo
		want     string
	}{
		{"embedded", "v1.2.3", &debug.BuildInfo{Main: debug.Module{Version: "v2.0.0"}, Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "deadbeef"}}}, "v1.2.3 (deadbeef)"},
		{"embedded devel", "devel", &debug.BuildInfo{Main: debug.Module{Version: "v2.0.0"}}, "devel (unknown)"},
		{"embedded without build info", "v1.2.3", nil, "v1.2.3 (unknown)"},
		{"module tag", "", &debug.BuildInfo{Main: debug.Module{Version: "v1.2.3"}}, "v1.2.3 (unknown)"},
		{"pseudo version", "", &debug.BuildInfo{Main: debug.Module{Version: "v0.0.0-20260927000000-0123456789ab"}, Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "0123456789abcdef"}}}, "v0.0.0-20260927000000-0123456789ab (0123456789abcdef)"},
		{"empty module version", "", &debug.BuildInfo{}, "devel (unknown)"},
		{"local build", "", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, "devel (unknown)"},
		{"no build info", "", nil, "devel (unknown)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveVersion(tc.embedded, tc.info); got != tc.want {
				t.Fatalf("version: got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestUserPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
		args []string
		want string
	}{
		{"default", "", nil, "me"},
		{"environment", "env@example.com", nil, "env@example.com"},
		{"option", "env@example.com", []string{"--user", "cli@example.com"}, "cli@example.com"},
		{"equals", "env@example.com", []string{"--user=cli@example.com"}, "cli@example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GMAIL_FILTER_SYNC_USER", tc.env)
			path := filepath.Join(t.TempDir(), "filter.jsonnet")
			save(t, path, "{ filters: [] }")
			called := false
			api := func(method, user, id string, body *Filter) (map[string]any, error) {
				called = true
				if user != tc.want {
					t.Fatalf("user: got %q, want %q", user, tc.want)
				}
				return map[string]any{}, nil
			}
			args := append(tc.args, "--file", path, "diff")
			if err := run(args, api, io.Discard, io.Discard); err != nil {
				t.Fatal(err)
			}
			if !called {
				t.Fatal("API was not called")
			}
		})
	}
}

func TestCLI(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("GMAIL_FILTER_SYNC_FILE", "")
	calls := []string{}
	api := func(method, user, id string, f *Filter) (map[string]any, error) {
		calls = append(calls, method)
		return map[string]any{}, nil
	}
	if e := run([]string{"import"}, api, io.Discard, io.Discard); e != nil {
		t.Fatal(e)
	}
	path, _ := defaultFile()
	if _, e := load(path); e != nil {
		t.Fatal(e)
	}
	var out bytes.Buffer
	if e := run([]string{"diff"}, api, &out, io.Discard); e != nil {
		t.Fatal(e)
	}
	if out.Len() != 0 {
		t.Fatal(out.String())
	}
	if e := run([]string{"apply"}, api, io.Discard, io.Discard); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(calls, []string{"list", "list", "list"}) {
		t.Fatal(calls)
	}
	for _, args := range [][]string{{"plan"}, {"sync"}, {"apply", "--apply"}, {"diff", "--allow-empty"}, {"diff", "--backup-dir", dir}, {"--user"}, {"--user="}, {"--user", "", "diff"}, {"diff", "a", "b"}} {
		if e := run(args, api, io.Discard, io.Discard); e == nil {
			t.Fatal("accepted", args)
		}
	}
	if e := run([]string{"import"}, api, io.Discard, io.Discard); e == nil {
		t.Fatal("overwritten")
	}
	if e := run([]string{"diff", "--force"}, api, io.Discard, io.Discard); e == nil {
		t.Fatal("accepted --force outside import")
	}
	if e := run([]string{"import", "--force"}, api, io.Discard, io.Discard); e != nil {
		t.Fatal("import --force failed:", e)
	}
}

func TestFilePrecedence(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	defaultPath, err := defaultFile()
	if err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(dir, "environment.jsonnet")
	cliPath := filepath.Join(dir, "command-line.jsonnet")
	for _, path := range []string{defaultPath, envPath, cliPath} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		save(t, path, "{ filters: [] }")
	}
	for _, tc := range []struct {
		name string
		env  string
		args []string
		want string
	}{
		{"default", "", []string{"diff"}, defaultPath},
		{"environment", envPath, []string{"diff"}, envPath},
		{"option overrides environment", envPath, []string{"--file", cliPath, "diff"}, cliPath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GMAIL_FILTER_SYNC_FILE", tc.env)
			for _, path := range []string{defaultPath, envPath, cliPath} {
				save(t, path, "invalid")
			}
			save(t, tc.want, "{ filters: [] }")
			called := false
			api := func(method, user, id string, body *Filter) (map[string]any, error) {
				called = true
				return map[string]any{}, nil
			}
			if err := run(tc.args, api, io.Discard, io.Discard); err != nil {
				t.Fatal(err)
			}
			if !called {
				t.Fatalf("file %q was not loaded", tc.want)
			}
		})
	}
	t.Setenv("GMAIL_FILTER_SYNC_FILE", envPath)
	if err := run([]string{"--file", cliPath, "diff", "extra.jsonnet"}, nil, io.Discard, io.Discard); err == nil {
		t.Fatal("accepted positional file path")
	}
}

func TestDefaultFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, xdg := range []string{"", "relative", "/tmp/custom"} {
		t.Setenv("XDG_CONFIG_HOME", xdg)
		p, e := defaultFile()
		base := xdg
		if !filepath.IsAbs(base) {
			base = filepath.Join(os.Getenv("HOME"), ".config")
		}
		if e != nil || p != filepath.Join(base, "gmail-filter-sync", "filter.jsonnet") {
			t.Fatal(p, e)
		}
	}
}

func TestEditor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "space $dollar; file.jsonnet")
	t.Setenv("EDITOR", `printf '{ filters: [] }\n' >`)
	if e := edit(path); e != nil {
		t.Fatal(e)
	}
	if _, e := load(path); e != nil {
		t.Fatal(e)
	}
	t.Setenv("EDITOR", "")
	if e := edit(path); e == nil {
		t.Fatal("missing editor")
	}
	t.Setenv("EDITOR", "false")
	if e := edit(path); e == nil {
		t.Fatal("editor failure")
	}
}

func TestGWS(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gws")
	// シェルスクリプトのインデントを保持するテストデータなので、この文字列だけ除外する
	// editorconfig-checker-disable
	script := `#!/bin/sh
if [ "$1 $2 $3 $4" = 'gmail users labels list' ]; then
  printf '{"labels":[{"id":"Label_123","name":"Projects/Go"}]}'
  exit 0
fi
[ "$1 $2 $3 $4" = 'gmail users settings filters' ] || exit 3
case "$5" in
list) printf '{"filter":[]}' ;;
create) printf '{"id":"new"}' ;;
delete) exit 0 ;;
esac
`
	// editorconfig-checker-enable
	if e := os.WriteFile(path, []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, e := current("me", gws); e != nil {
		t.Fatal(e)
	}
	labels, e := getLabels("me", gws)
	if e != nil || labels.nameToID["Projects/Go"] != "Label_123" {
		t.Fatalf("labels.list route failed: %#v, %v", labels, e)
	}
	f := rule(t)
	if data, e := gws("create", "me", "", &f); e != nil || data["id"] != "new" {
		t.Fatal(data, e)
	}
	if _, e := gws("delete", "me", "id", nil); e != nil {
		t.Fatal(e)
	}
	save(t, path, "#!/bin/sh\nexit 1\n")
	if _, e := gws("list", "me", "", nil); e == nil {
		t.Fatal("ignored failure")
	}
	save(t, path, "#!/bin/sh\nexit 0\n")
	if _, e := gws("list", "me", "", nil); e == nil {
		t.Fatal("empty response")
	}
}

func TestMapLabelNames(t *testing.T) {
	catalog := labelCatalog{
		nameToID: map[string]string{"Inbox": "INBOX", "Projects/Go": "Label_6616939519625642916", "STARRED": "STARRED"},
		idToName: map[string]string{"INBOX": "Inbox", "Label_6616939519625642916": "Projects/Go", "STARRED": "STARRED"},
	}
	configured, err := normalize(map[string]any{
		"criteria": map[string]any{"from": "news@example.com"},
		"action":   map[string]any{"removeLabels": []any{"Inbox"}, "addLabels": []any{"Projects/Go"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	apiFilters, err := mapLabels([]Filter{configured}, catalog, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := apiFilters[0].Action["addLabelIds"].([]string)[0]; got != "Label_6616939519625642916" {
		t.Fatalf("label name was not resolved: %q", got)
	}
	if _, exists := apiFilters[0].Action["addLabels"]; exists {
		t.Fatal("config label name remained in API filter")
	}
	back, err := mapLabels(apiFilters, catalog, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := back[0].Action["addLabels"].([]string)[0]; got != "Projects/Go" {
		t.Fatalf("label name was not restored: %q", got)
	}
	if _, err = mapLabels([]Filter{configured}, labelCatalog{}, true); err == nil || !strings.Contains(err.Error(), "unknown Gmail label") {
		t.Fatalf("expected unknown label error, got %v", err)
	}
}

func TestLabelMappingPreservesOrder(t *testing.T) {
	t.Setenv("GMAIL_FILTER_SYNC_DIFF_FILTER", "")
	path := filepath.Join(t.TempDir(), "filters.jsonnet")
	api := func(method, user, id string, body *Filter) (map[string]any, error) {
		switch method {
		case "list":
			return map[string]any{"filter": []any{map[string]any{
				"id":       "existing",
				"criteria": map[string]any{"from": "news@example.com"},
				"action":   map[string]any{"addLabelIds": []any{"Label_1", "STARRED"}},
			}}}, nil
		case "labels.list":
			return map[string]any{"labels": []any{
				map[string]any{"id": "Label_1", "name": "ZZZ"},
				map[string]any{"id": "STARRED", "name": "STARRED"},
			}}, nil
		default:
			t.Fatalf("unexpected API call: %s", method)
			return nil, nil
		}
	}
	if err := run([]string{"--file", path, "import"}, api, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run([]string{"--file", path, "diff"}, api, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("unchanged import produced a diff: %s", out.String())
	}
	if err := run([]string{"--file", path, "apply"}, api, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestConflictingLabelNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "filters.jsonnet")
	save(t, path, "{ filters: [{ criteria: { from: 'news@example.com' }, action: { addLabels: ['STARRED'], removeLabels: ['STARRED'] } }] }")
	if _, err := load(path); err == nil {
		t.Fatal("accepted a label in both addLabels and removeLabels")
	}
	save(t, path, "{ filters: [{ criteria: { from: 'news@example.com' }, action: { addLabels: ['STARRED'], removeLabelIds: ['STARRED'] } }] }")
	filters, err := load(path)
	if err != nil {
		t.Fatal(err)
	}
	catalog := labelCatalog{
		nameToID: map[string]string{"STARRED": "STARRED"},
		idToName: map[string]string{"STARRED": "STARRED"},
	}
	if _, err := mapLabels(filters, catalog, true); err == nil {
		t.Fatal("accepted a label in both addLabels and removeLabelIds")
	}
}

func TestRunUsesLabelNamesForDiffAndApply(t *testing.T) {
	t.Setenv("GMAIL_FILTER_SYNC_DIFF_FILTER", "")
	path := filepath.Join(t.TempDir(), "filters.jsonnet")
	save(t, path, "{ filters: [{ criteria: { from: 'news@example.com' }, action: { addLabels: ['Projects/Go'], removeLabels: ['Inbox'] } }] }\n")
	calls := []string{}
	state := []any{}
	api := func(method, user, id string, body *Filter) (map[string]any, error) {
		calls = append(calls, method)
		switch method {
		case "list":
			return map[string]any{"filter": state}, nil
		case "labels.list":
			return map[string]any{"labels": []any{
				map[string]any{"id": "INBOX", "name": "Inbox"},
				map[string]any{"id": "Label_6616939519625642916", "name": "Projects/Go"},
			}}, nil
		case "create":
			if got := body.Action["addLabelIds"].([]string)[0]; got != "Label_6616939519625642916" {
				t.Errorf("create got label %q", got)
			}
			if got := body.Action["removeLabelIds"].([]string)[0]; got != "INBOX" {
				t.Errorf("create got label %q", got)
			}
			created := *body
			created.ID = "created"
			state = append(state, raw(created))
			return map[string]any{"id": "created"}, nil
		default:
			return nil, errors.New("unexpected API call: " + method)
		}
	}
	var out bytes.Buffer
	if err := run([]string{"--file", path, "diff"}, api, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Projects/Go") || !strings.Contains(out.String(), "Inbox") || strings.Contains(out.String(), "Label_661") {
		t.Fatalf("diff should use label names: %s", out.String())
	}
	if err := run([]string{"--file", path, "apply"}, api, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
}
