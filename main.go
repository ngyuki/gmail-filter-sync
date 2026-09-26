package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"

	"github.com/google/go-jsonnet"
)

var version string

type Filter struct {
	ID       string         `json:"id,omitempty"`
	Criteria map[string]any `json:"criteria"`
	Action   map[string]any `json:"action"`
}

type Document struct {
	Filters []Filter `json:"filters"`
}

type Changes struct {
	Create []Filter `json:"create"`
	Delete []Filter `json:"delete"`
}

type API func(method, user, id string, body *Filter) (map[string]any, error)

func normalize(raw any) (Filter, error) {
	f := Filter{Criteria: map[string]any{}, Action: map[string]any{}}
	obj, ok := raw.(map[string]any)
	if !ok {
		return f, errors.New("invalid filter")
	}
	for k := range obj {
		if k != "id" && k != "criteria" && k != "action" {
			return f, fmt.Errorf("invalid filter field: %s", k)
		}
	}
	for _, section := range []string{"criteria", "action"} {
		values := map[string]any{}
		if v, exists := obj[section]; exists {
			values, ok = v.(map[string]any)
			if !ok {
				return f, fmt.Errorf("invalid %s fields", section)
			}
		}
		target := f.Criteria
		if section == "action" {
			target = f.Action
		}
		for k, v := range values {
			kind := ""
			if section == "criteria" {
				switch k {
				case "from", "to", "subject", "query", "negatedQuery", "sizeComparison":
					kind = "string"
				case "hasAttachment", "excludeChats":
					kind = "bool"
				case "size":
					kind = "size"
				}
			} else {
				switch k {
				case "forward":
					kind = "string"
				case "addLabelIds", "removeLabelIds", "addLabels", "removeLabels":
					kind = "list"
				}
			}
			switch kind {
			case "string":
				s, ok := v.(string)
				if !ok {
					return f, fmt.Errorf("invalid type for %s.%s", section, k)
				}
				if k == "sizeComparison" {
					if s == "unspecified" {
						continue
					}
					if s != "smaller" && s != "larger" {
						return f, errors.New("invalid sizeComparison")
					}
				}
				if s != "" {
					target[k] = s
				}
			case "bool":
				b, ok := v.(bool)
				if !ok {
					return f, fmt.Errorf("invalid type for %s", k)
				}
				if b {
					target[k] = b
				}
			case "size":
				var n float64
				switch x := v.(type) {
				case int:
					n = float64(x)
				case float64:
					n = x
				default:
					return f, errors.New("size must be a nonnegative int32")
				}
				if n < 0 || n > 2147483647 || n != float64(int64(n)) {
					return f, errors.New("size must be a nonnegative int32")
				}
				target[k] = int64(n)
			case "list":
				list, ok := v.([]any)
				if !ok {
					return f, fmt.Errorf("invalid label IDs in %s", k)
				}
				set := map[string]bool{}
				for _, item := range list {
					s, ok := item.(string)
					if !ok || s == "" {
						return f, fmt.Errorf("invalid label IDs in %s", k)
					}
					set[s] = true
				}
				result := []string{}
				for s := range set {
					result = append(result, s)
				}
				sort.Strings(result)
				if len(result) > 0 {
					target[k] = result
				}
			default:
				return f, fmt.Errorf("invalid %s field: %s", section, k)
			}
		}
	}
	if _, ok := f.Action["addLabels"]; ok {
		if _, duplicate := f.Action["addLabelIds"]; duplicate {
			return f, errors.New("use either addLabels or addLabelIds")
		}
	}
	if _, ok := f.Action["removeLabels"]; ok {
		if _, duplicate := f.Action["removeLabelIds"]; duplicate {
			return f, errors.New("use either removeLabels or removeLabelIds")
		}
	}
	_, size := f.Criteria["size"]
	_, comparison := f.Criteria["sizeComparison"]
	if size != comparison {
		return f, errors.New("size and sizeComparison must be specified together")
	}
	if len(f.Action) == 0 {
		return f, errors.New("a filter must have an action")
	}
	if err := validateLabelActions(f.Action); err != nil {
		return f, err
	}
	return f, nil
}

func validateLabelActions(action map[string]any) error {
	for _, pair := range [][2]string{{"addLabelIds", "removeLabelIds"}, {"addLabels", "removeLabels"}} {
		add := labelReferences(action[pair[0]])
		remove := labelReferences(action[pair[1]])
		for _, a := range add {
			for _, r := range remove {
				if a == r {
					return errors.New("a label cannot be both added and removed")
				}
			}
		}
	}
	return nil
}

func labelReferences(value any) []string {
	if labels, ok := value.([]string); ok {
		return labels
	}
	items, _ := value.([]any)
	labels := make([]string, 0, len(items))
	for _, item := range items {
		if label, ok := item.(string); ok {
			labels = append(labels, label)
		}
	}
	return labels
}

func key(f Filter) string {
	f.ID = ""
	b, _ := json.Marshal(f)
	return string(b)
}

func document(filters []Filter) Document {
	result := make([]Filter, len(filters))
	copy(result, filters)
	for i := range result {
		result[i].ID = ""
	}
	sort.Slice(result, func(i, j int) bool { return key(result[i]) < key(result[j]) })
	return Document{result}
}

func load(path string) ([]Filter, error) {
	vm := jsonnet.MakeVM()
	evaluated, err := vm.EvaluateFile(path)
	if err != nil {
		return nil, err
	}
	var obj map[string]any
	if err = json.Unmarshal([]byte(evaluated), &obj); err != nil {
		return nil, err
	}
	list, ok := obj["filters"].([]any)
	if !ok || len(obj) != 1 {
		return nil, errors.New("expected an object containing only a filters array")
	}
	if len(list) > 1000 {
		return nil, errors.New("at most 1000 filters are allowed")
	}
	rules := []Filter{}
	for _, raw := range list {
		f, e := normalize(raw)
		if e != nil {
			return nil, e
		}
		rules = append(rules, f)
	}
	return rules, nil
}

func gws(method, user, id string, body *Filter) (map[string]any, error) {
	params := map[string]string{"userId": user}
	if id != "" {
		params["id"] = id
	}
	p, _ := json.Marshal(params)
	var args []string
	if method == "labels.list" {
		args = []string{"gmail", "users", "labels", "list"}
	} else {
		args = []string{"gmail", "users", "settings", "filters", method}
	}
	args = append(args, "--params", string(p), "--format", "json")
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		args = append(args, "--json", string(b))
	}
	cmd := exec.Command("gws", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gws %s failed: %w: %s", method, err, strings.TrimSpace(stderr.String()))
	}
	if len(bytes.TrimSpace(out)) == 0 {
		if method == "delete" {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("gws %s returned an empty response", method)
	}
	var data map[string]any
	if err = json.Unmarshal(out, &data); err != nil {
		return nil, err
	}
	if data == nil {
		return nil, errors.New("invalid API response")
	}
	if _, ok := data["error"]; ok {
		return nil, errors.New("API returned an error")
	}
	return data, nil
}

type labelCatalog struct {
	nameToID map[string]string
	idToName map[string]string
}

func getLabels(user string, api API) (labelCatalog, error) {
	data, err := api("labels.list", user, "", nil)
	if err != nil {
		return labelCatalog{}, err
	}
	raw, ok := data["labels"].([]any)
	if !ok {
		return labelCatalog{}, errors.New("invalid labels list response")
	}
	catalog := labelCatalog{map[string]string{}, map[string]string{}}
	for _, item := range raw {
		label, ok := item.(map[string]any)
		if !ok {
			return catalog, errors.New("invalid label entry")
		}
		id, idOK := label["id"].(string)
		name, nameOK := label["name"].(string)
		if !idOK || !nameOK || id == "" || name == "" {
			return catalog, errors.New("invalid label entry")
		}
		if _, exists := catalog.nameToID[name]; exists {
			return catalog, fmt.Errorf("duplicate Gmail label name: %s", name)
		}
		catalog.nameToID[name] = id
		catalog.idToName[id] = name
	}
	return catalog, nil
}

func mapLabels(filters []Filter, catalog labelCatalog, toIDs bool) ([]Filter, error) {
	result := make([]Filter, len(filters))
	for i, filter := range filters {
		b, _ := json.Marshal(filter)
		var clone Filter
		if err := json.Unmarshal(b, &clone); err != nil {
			return nil, err
		}
		for _, pair := range [][2]string{{"addLabels", "addLabelIds"}, {"removeLabels", "removeLabelIds"}} {
			from, to := pair[0], pair[1]
			if !toIDs {
				from, to = to, from
			}
			refs, exists := clone.Action[from]
			if !exists {
				continue
			}
			names, ok := refs.([]any)
			if !ok {
				if strings, yes := refs.([]string); yes {
					names = make([]any, len(strings))
					for j, v := range strings {
						names[j] = v
					}
					ok = true
				}
			}
			if !ok {
				return nil, fmt.Errorf("invalid label references in %s", from)
			}
			mapped := []string{}
			for _, ref := range names {
				value, ok := ref.(string)
				if !ok {
					return nil, fmt.Errorf("invalid label reference in %s", from)
				}
				var converted string
				if toIDs {
					converted, ok = catalog.nameToID[value]
				} else {
					converted, ok = catalog.idToName[value]
				}
				if !ok {
					return nil, fmt.Errorf("unknown Gmail label: %s", value)
				}
				mapped = append(mapped, converted)
			}
			sort.Strings(mapped)
			delete(clone.Action, from)
			clone.Action[to] = mapped
		}
		if err := validateLabelActions(clone.Action); err != nil {
			return nil, err
		}
		result[i] = clone
	}
	return result, nil
}

func requiresLabels(filters []Filter) bool {
	for _, filter := range filters {
		for _, key := range []string{"addLabels", "removeLabels", "addLabelIds", "removeLabelIds"} {
			if _, ok := filter.Action[key]; ok {
				return true
			}
		}
	}
	return false
}

func current(user string, api API) ([]Filter, error) {
	data, err := api("list", user, "", nil)
	if err != nil {
		return nil, err
	}
	for k := range data {
		if k != "filter" {
			return nil, errors.New("unexpected filter list response")
		}
	}
	raw, exists := data["filter"]
	if !exists {
		return []Filter{}, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, errors.New("invalid filter list response")
	}
	result := []Filter{}
	ids := map[string]bool{}
	for _, r := range list {
		f, e := normalize(r)
		if e != nil {
			return nil, e
		}
		id, ok := r.(map[string]any)["id"].(string)
		if !ok || id == "" || ids[id] {
			return nil, errors.New("missing or duplicate remote filter ID")
		}
		ids[id] = true
		f.ID = id
		result = append(result, f)
	}
	return result, nil
}

func diff(desired, existing []Filter) Changes {
	remaining := append([]Filter{}, existing...)
	additions := []Filter{}
	for _, rule := range desired {
		match := -1
		for i, old := range remaining {
			if key(old) == key(rule) {
				match = i
				break
			}
		}
		if match < 0 {
			additions = append(additions, rule)
		} else {
			remaining = append(remaining[:match], remaining[match+1:]...)
		}
	}
	return Changes{additions, remaining}
}

func writeJsonnet(path string, data any, force ...bool) error {
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if len(force) > 0 && force[0] {
		dir := filepath.Dir(path)
		temp, err := os.CreateTemp(dir, ".filter-*.jsonnet")
		if err != nil {
			return err
		}
		defer os.Remove(temp.Name())
		if err = temp.Chmod(0600); err != nil {
			temp.Close()
			return err
		}
		if _, err = temp.Write(b); err != nil {
			temp.Close()
			return err
		}
		if err = temp.Close(); err != nil {
			return err
		}
		return os.Rename(temp.Name(), path)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func printChanges(out io.Writer, c Changes) error {
	if len(c.Create)+len(c.Delete) == 0 {
		return nil
	}
	var diffOutput bytes.Buffer
	if _, err := fmt.Fprintln(&diffOutput, "--- current"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(&diffOutput, "+++ desired"); err != nil {
		return err
	}
	deleted, err := formatDiffRules(c.Delete)
	if err != nil {
		return err
	}
	added, err := formatDiffRules(c.Create)
	if err != nil {
		return err
	}
	oldLines, newLines := flattenDiffRules(deleted), flattenDiffRules(added)
	lines := diffLines(oldLines, newLines)
	oldCount, newCount := 0, 0
	for _, line := range lines {
		if line[0] != '+' {
			oldCount++
		}
		if line[0] != '-' {
			newCount++
		}
	}
	oldStart, newStart := 1, 1
	if oldCount == 0 {
		oldStart = 0
	}
	if newCount == 0 {
		newStart = 0
	}
	if _, err := fmt.Fprintf(&diffOutput, "@@ -%d,%d +%d,%d @@\n", oldStart, oldCount, newStart, newCount); err != nil {
		return err
	}
	for _, line := range lines {
		if _, err := fmt.Fprintln(&diffOutput, line); err != nil {
			return err
		}
	}
	filter := os.Getenv("GMAIL_FILTER_SYNC_DIFF_FILTER")
	if filter == "" {
		_, err := io.Copy(out, &diffOutput)
		return err
	}
	cmd := exec.Command("/bin/sh", "-c", filter)
	cmd.Stdin = &diffOutput
	cmd.Stdout = out
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("diff filter failed: %w", err)
	}
	return nil
}

func formatDiffRules(rules []Filter) ([][]string, error) {
	formatted := make([][]string, 0, len(rules))
	for _, rule := range rules {
		visible := struct {
			Criteria map[string]any `json:"criteria"`
			Action   map[string]any `json:"action"`
		}{rule.Criteria, rule.Action}
		b, err := json.MarshalIndent(visible, "", "  ")
		if err != nil {
			return nil, err
		}
		lines := strings.Split(string(b), "\n")
		if len(formatted) > 0 {
			formatted = append(formatted, []string{""})
		}
		formatted = append(formatted, lines)
	}
	return formatted, nil
}

func flattenDiffRules(entries [][]string) []string {
	var result []string
	for _, lines := range entries {
		result = append(result, lines...)
	}
	return result
}

func diffLines(old, new []string) []string {
	common := longestCommonLines(old, new)
	result := make([]string, 0, len(old)+len(new))
	oldIndex, newIndex := 0, 0
	for _, line := range common {
		for oldIndex < len(old) && old[oldIndex] != line {
			result = append(result, "-"+old[oldIndex])
			oldIndex++
		}
		for newIndex < len(new) && new[newIndex] != line {
			result = append(result, "+"+new[newIndex])
			newIndex++
		}
		result = append(result, " "+line)
		oldIndex++
		newIndex++
	}
	for oldIndex < len(old) {
		result = append(result, "-"+old[oldIndex])
		oldIndex++
	}
	for newIndex < len(new) {
		result = append(result, "+"+new[newIndex])
		newIndex++
	}
	return result
}

func longestCommonLines(old, new []string) []string {
	if len(old) == 0 || len(new) == 0 {
		return nil
	}
	if len(old) == 1 {
		for _, line := range new {
			if old[0] == line {
				return []string{line}
			}
		}
		return nil
	}
	middle := len(old) / 2
	forward := lcsLengths(old[:middle], new)
	backward := lcsLengths(reverseLines(old[middle:]), reverseLines(new))
	split, best := 0, -1
	for i := 0; i <= len(new); i++ {
		if score := forward[i] + backward[len(new)-i]; score > best {
			split, best = i, score
		}
	}
	result := longestCommonLines(old[:middle], new[:split])
	return append(result, longestCommonLines(old[middle:], new[split:])...)
}

func lcsLengths(old, new []string) []int {
	lengths := make([]int, len(new)+1)
	for _, oldLine := range old {
		previous := 0
		for j, newLine := range new {
			before := lengths[j+1]
			if oldLine == newLine {
				lengths[j+1] = previous + 1
			} else if lengths[j] > lengths[j+1] {
				lengths[j+1] = lengths[j]
			}
			previous = before
		}
	}
	return lengths
}

func reverseLines(lines []string) []string {
	reversed := make([]string, len(lines))
	for i := range lines {
		reversed[len(lines)-1-i] = lines[i]
	}
	return reversed
}

func apply(desired, existing []Filter, user string, allowEmpty bool, api API, out, log io.Writer, catalogs ...labelCatalog) error {
	changes := diff(desired, existing)
	display := changes
	if len(catalogs) > 0 {
		var err error
		display.Create, err = mapLabels(changes.Create, catalogs[0], false)
		if err != nil {
			return err
		}
		display.Delete, err = mapLabels(changes.Delete, catalogs[0], false)
		if err != nil {
			return err
		}
	}
	if err := printChanges(out, display); err != nil {
		return err
	}
	if len(changes.Create)+len(changes.Delete) == 0 {
		return nil
	}
	if len(desired) == 0 && !allowEmpty {
		return errors.New("refusing to delete all filters without --allow-empty")
	}
	if len(existing)+len(changes.Create) > 1000 {
		return errors.New("create-before-delete would exceed 1000 filters; reduce filters first")
	}
	for _, rule := range changes.Create {
		data, e := api("create", user, "", &rule)
		if e != nil {
			return e
		}
		id, ok := data["id"].(string)
		if !ok || id == "" {
			return errors.New("create response has no ID; inspect remote state before retrying")
		}
		fmt.Fprintln(log, "Created:", id)
	}
	for _, rule := range changes.Delete {
		if _, err := api("delete", user, rule.ID, nil); err != nil {
			return err
		}
		fmt.Fprintln(log, "Deleted:", rule.ID)
	}
	latest, err := current(user, api)
	if err != nil {
		return err
	}
	remaining := diff(desired, latest)
	if len(remaining.Create)+len(remaining.Delete) > 0 {
		return errors.New("post-apply verification failed; inspect the remote filters")
	}
	fmt.Fprintln(log, "Apply complete")
	return nil
}

func defaultFile() (string, error) {
	config := os.Getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(config) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		config = filepath.Join(home, ".config")
	}
	return filepath.Join(config, "gmail-filter-sync", "filter.jsonnet"), nil
}

func edit(file string) error {
	editor := os.Getenv("EDITOR")
	if strings.TrimSpace(editor) == "" {
		return errors.New("EDITOR is not set")
	}
	absolute, err := filepath.Abs(file)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(absolute), 0700); err != nil {
		return err
	}
	cmd := exec.Command("/bin/sh", "-c", editor+` "$1"`, "gmail-filter-sync-editor", absolute)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err = cmd.Run(); err != nil {
		return fmt.Errorf("editor failed: %w", err)
	}
	return nil
}

// editorconfig-checker-disable
const usage = `Manage Gmail filters as Jsonnet through gws.

Usage:
  gmail-filter-sync [--user USER] [--file FILE] import [--force]
  gmail-filter-sync [--user USER] [--file FILE] diff
  gmail-filter-sync [--user USER] [--file FILE] apply [--allow-empty]
  gmail-filter-sync [--file FILE] edit

The file is selected in this order: --file, GMAIL_FILTER_SYNC_FILE,
then $XDG_CONFIG_HOME/gmail-filter-sync/filter.jsonnet.
If XDG_CONFIG_HOME is unset, empty, or relative, use ~/.config instead.
edit opens FILE with $EDITOR; it does not apply changes.

Options:
  --user USER       Gmail user ID (overrides GMAIL_FILTER_SYNC_USER)
  --file FILE       Filter configuration file (overrides GMAIL_FILTER_SYNC_FILE)
  --force           Overwrite existing import file (import only)
  --allow-empty     Allow removal of all filters (apply only)
  --version         Show version and Git revision
  --help, -h        Show help

Environment:
  GMAIL_FILTER_SYNC_USER         Default Gmail user ID (default: me)
  GMAIL_FILTER_SYNC_FILE         Default filter configuration file
  GMAIL_FILTER_SYNC_DIFF_FILTER  Shell command to format diff output
`

// editorconfig-checker-enable

func run(args []string, api API, out, log io.Writer) error {
	user := os.Getenv("GMAIL_FILTER_SYNC_USER")
	if user == "" {
		user = "me"
	}
	allowEmpty, force, help := false, false, false
	showVersion := false
	fileOption := ""
	pos := []string{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		name, value, hasValue := strings.Cut(arg, "=")
		switch name {
		case "--version":
			if hasValue {
				return errors.New("unexpected option value")
			}
			showVersion = true
		case "--help", "-h":
			if hasValue {
				return errors.New("unexpected option value")
			}
			help = true
		case "--allow-empty":
			if hasValue {
				return errors.New("unexpected option value")
			}
			allowEmpty = true
		case "--force":
			if hasValue {
				return errors.New("unexpected option value")
			}
			force = true
		case "--user":
			if !hasValue {
				i++
				if i >= len(args) || strings.HasPrefix(args[i], "--") {
					return fmt.Errorf("missing value for %s", name)
				}
				value = args[i]
			}
			if value == "" {
				return errors.New("missing value for --user")
			}
			user = value
		case "--file":
			if !hasValue {
				i++
				if i >= len(args) || strings.HasPrefix(args[i], "--") {
					return fmt.Errorf("missing value for %s", name)
				}
				value = args[i]
			}
			if value == "" {
				return errors.New("missing value for --file")
			}
			fileOption = value
		default:
			if strings.HasPrefix(arg, "-") {
				return fmt.Errorf("unknown option: %s", arg)
			}
			pos = append(pos, arg)
		}
	}
	if help {
		_, err := fmt.Fprint(out, usage)
		return err
	}
	if showVersion {
		info, _ := debug.ReadBuildInfo()
		_, err := fmt.Fprintf(out, "gmail-filter-sync %s\n", resolveVersion(version, info))
		return err
	}
	if len(pos) != 1 {
		return errors.New("expected import, diff, apply, or edit; use --help")
	}
	command := pos[0]
	if command != "import" && command != "diff" && command != "apply" && command != "edit" {
		return errors.New("expected import, diff, apply, or edit; use --help")
	}
	if command != "apply" && allowEmpty {
		return errors.New("--allow-empty requires apply")
	}
	if command != "import" && force {
		return errors.New("--force requires import")
	}
	var file string
	var err error
	if fileOption != "" {
		file = fileOption
	} else if envFile := os.Getenv("GMAIL_FILTER_SYNC_FILE"); envFile != "" {
		file = envFile
	} else {
		file, err = defaultFile()
		if err != nil {
			return err
		}
	}
	if command == "edit" {
		return edit(file)
	}
	if command == "import" {
		rules, e := current(user, api)
		if e != nil {
			return e
		}
		if requiresLabels(rules) {
			catalog, err := getLabels(user, api)
			if err != nil {
				return err
			}
			rules, err = mapLabels(rules, catalog, false)
			if err != nil {
				return err
			}
		}
		if e = os.MkdirAll(filepath.Dir(file), 0700); e != nil {
			return e
		}
		return writeJsonnet(file, document(rules), force)
	}
	desired, err := load(file)
	if err != nil {
		return err
	}
	existing, err := current(user, api)
	if err != nil {
		return err
	}
	var catalog *labelCatalog
	if requiresLabels(desired) || requiresLabels(existing) {
		labels, e := getLabels(user, api)
		if e != nil {
			return e
		}
		catalog = &labels
		desired, e = mapLabels(desired, labels, true)
		if e != nil {
			return e
		}
	}
	if command == "diff" {
		changes := diff(desired, existing)
		if catalog != nil {
			changes.Create, err = mapLabels(changes.Create, *catalog, false)
			if err != nil {
				return err
			}
			changes.Delete, err = mapLabels(changes.Delete, *catalog, false)
			if err != nil {
				return err
			}
		}
		return printChanges(out, changes)
	}
	if catalog != nil {
		return apply(desired, existing, user, allowEmpty, api, out, log, *catalog)
	}
	return apply(desired, existing, user, allowEmpty, api, out, log)
}

func resolveVersion(embedded string, info *debug.BuildInfo) string {
	revision := "unknown"
	if info != nil {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				revision = setting.Value
				break
			}
		}
	}
	if embedded != "" {
		return fmt.Sprintf("%s (%s)", embedded, revision)
	}
	resolved := "devel"
	if info != nil && info.Main.Version != "" && info.Main.Version != "(devel)" {
		resolved = info.Main.Version
	}
	return fmt.Sprintf("%s (%s)", resolved, revision)
}

func main() {
	if err := run(os.Args[1:], gws, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
