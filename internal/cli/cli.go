// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/peytonr18/azure-init-go/kvp"
)

const defaultAgent = "azure-init-go/0.1.0"

type usageError struct{ message string }

func (e *usageError) Error() string { return e.message }
func usage(format string, args ...interface{}) error {
	return &usageError{fmt.Sprintf(format, args...)}
}

type commandSpec struct {
	min, max int
	flags    string
	usage    string
	summary  string
}

var commands = map[string]commandSpec{
	"info":               {0, 0, "", "", "Print store metadata"},
	"dump":               {0, 0, "parse name kind", "", "Print every record in pool order"},
	"entries":            {0, 0, "", "", "Print key=last_value entries sorted by key"},
	"read":               {1, 1, "", "<KEY>", "Print the last value for KEY (exit 1 if missing)"},
	"write":              {2, 2, "append", "<KEY> <VALUE>", "Write a record; --append preserves prior values"},
	"emit":               {0, 0, "name message vm-id agent", "--name <NAME> --message <MESSAGE>", "Emit a standalone diagnostic"},
	"load":               {0, 0, "file", "", "Replace the pool from KEY=VALUE lines"},
	"append-multiple":    {0, 0, "file", "", "Append KEY=VALUE lines"},
	"delete":             {1, 1, "", "<KEY>", "Delete every record with KEY"},
	"delete-multiple":    {1, -1, "", "<KEY>...", "Delete every record matching any KEY"},
	"clear":              {0, 0, "if-stale", "", "Clear the pool, optionally only when stale"},
	"is-stale":           {0, 0, "", "", "Print whether the pool is stale (exit 1 if not stale)"},
	"report-in-progress": {0, 0, "vm-id agent supporting-data", "", "Write an in-progress provisioning report"},
	"report-success":     {0, 0, "vm-id agent supporting-data", "", "Write a success provisioning report"},
	"report-failure":     {0, 0, "vm-id agent reason documentation-url supporting-data", "--reason <REASON>", "Write a failure provisioning report"},
	"help":               {0, 1, "", "[COMMAND]...", "Print this message or help for a command"},
}

const commandOrder = "info dump entries read write emit load append-multiple delete delete-multiple clear is-stale report-in-progress report-success report-failure help"
const globalOptions = "pool dir unsafe json text help"

var poolArguments = map[string]kvp.Pool{
	"external": kvp.External, "0": kvp.External, "guest": kvp.Guest, "1": kvp.Guest,
	"auto": kvp.Auto, "2": kvp.Auto, "auto-external": kvp.AutoExternal, "3": kvp.AutoExternal,
	"auto-internal": kvp.AutoInternal, "4": kvp.AutoInternal,
}

var optionDescriptions = map[string]string{
	"pool":              "<POOL>  external|guest|auto|auto-external|auto-internal (or 0 through 4; default: guest)",
	"dir":               "<DIR>  Pool directory (default: /var/lib/hyperv)",
	"unsafe":            "Allow full field widths that may be truncated in host transport",
	"json":              "Emit machine-readable JSON",
	"text":              "Emit human-readable text",
	"help":              "Print help",
	"parse":             "Decode diagnostics and reports, preserving raw entries in pool order",
	"name":              "<NAME>  Diagnostic name (substring filter for dump; requires --parse)",
	"kind":              "<KIND>  start|finish|event (requires --parse)",
	"append":            "Preserve existing records and duplicate keys",
	"if-stale":          "Clear only if modification time is at or before boot",
	"message":           "<MESSAGE>  Diagnostic payload",
	"vm-id":             "<VM_ID>  VM identity (default: DMI identity with Gen1 adjustment)",
	"agent":             "<AGENT>  Reporting agent (default: " + defaultAgent + ")",
	"file":              "<PATH>  Read KEY=VALUE lines from a file instead of stdin",
	"reason":            "<REASON>  Failure reason",
	"documentation-url": "<URL>  Failure documentation URL",
	"supporting-data":   "<DATA>  Repeatable key=value pairs; quote values to retain commas: \"key='a,b',other=c\"",
}

type services struct {
	newStore func(kvp.Pool, string, kvp.PoolMode) (commandStore, error)
	vmID     func() (string, error)
}

type arguments struct {
	command string
	args    []string
	values  map[string][]string
	bools   map[string]bool
}

func (a arguments) value(name, fallback string) string {
	values := a.values[name]
	if len(values) == 0 {
		return fallback
	}
	return values[0]
}

func parse(args []string) (arguments, error) {
	a := arguments{values: make(map[string][]string), bools: make(map[string]bool)}
	boolean := "unsafe json text parse append if-stale help"
	positional := false
	for i := 0; i < len(args); i++ {
		token := args[i]
		if a.command == "help" {
			a.args = append(a.args, token)
			continue
		}
		if !positional && token == "--" {
			positional = true
			continue
		}
		if !positional && (token == "-h" || token == "--help") {
			a.bools["help"] = true
			if a.command != "" {
				if _, ok := commands[a.command]; !ok {
					return a, usage("unknown command %q", a.command)
				}
			}
			return a, nil
		}
		if !positional && strings.HasPrefix(token, "-") && token != "-" {
			if !strings.HasPrefix(token, "--") {
				return a, usage("unknown option %s", token)
			}
			option := strings.SplitN(token[2:], "=", 2)
			name := option[0]
			allowed := globalOptions + " " + commands[a.command].flags
			if !containsWord(allowed, name) {
				return a, usage("--%s is not valid for %s", name, a.command)
			}
			if containsWord(boolean, name) {
				if len(option) != 1 || a.bools[name] {
					return a, usage("invalid or repeated flag --%s", name)
				}
				a.bools[name] = true
			} else {
				value := ""
				if len(option) == 2 {
					value = option[1]
				} else {
					i++
					if i >= len(args) || (strings.HasPrefix(args[i], "-") && args[i] != "-") {
						return a, usage("--%s requires a value (use --%s=VALUE for leading '-')", name, name)
					}
					value = args[i]
				}
				if len(a.values[name]) != 0 && name != "supporting-data" {
					return a, usage("repeated option --%s", name)
				}
				if name == "pool" {
					if _, ok := poolArguments[value]; !ok {
						return a, usage("invalid pool %q", value)
					}
				}
				if name == "kind" && value != "start" && value != "finish" && value != "event" {
					return a, usage("invalid diagnostic kind %q", value)
				}
				a.values[name] = append(a.values[name], value)
			}
		} else if a.command == "" {
			a.command = token
		} else {
			a.args = append(a.args, token)
		}
	}
	spec, ok := commands[a.command]
	if !ok {
		return a, usage("expected a command; use --help for available commands")
	}
	if len(a.args) < spec.min || (spec.max >= 0 && len(a.args) > spec.max) {
		return a, usage("incorrect arguments for %s", a.command)
	}
	if a.command == "help" && len(a.args) == 1 {
		if _, ok := commands[a.args[0]]; !ok {
			return a, usage("unknown help command %q", a.args[0])
		}
	}
	if a.bools["json"] && a.bools["text"] {
		return a, usage("--json and --text cannot be used together")
	}
	if a.command == "dump" {
		if (len(a.values["name"]) != 0 || len(a.values["kind"]) != 0) && !a.bools["parse"] {
			return a, usage("--name and --kind require --parse")
		}
	}
	for _, name := range map[string][]string{"emit": {"name", "message"}, "report-failure": {"reason"}}[a.command] {
		if len(a.values[name]) == 0 {
			return a, usage("--%s is required", name)
		}
	}
	return a, nil
}

func containsWord(words, word string) bool {
	for _, candidate := range strings.Fields(words) {
		if word == candidate {
			return true
		}
	}
	return false
}

// Run provides the thin command interface. Exit statuses match the Rust CLI:
// 0 success, 1 not found/not stale, 2 usage/validation, and 3 I/O failure.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return runWithServices(args, stdin, stdout, stderr, services{openStore, kvp.CurrentVMID})
}

func runWithServices(args []string, stdin io.Reader, stdout, stderr io.Writer, system services) int {
	a, err := parse(args)
	code := 0
	if err == nil {
		if a.bools["help"] || a.command == "help" {
			topic := a.command
			if topic == "help" {
				topic = ""
				if len(a.args) != 0 {
					topic = a.args[0]
				}
			}
			err = help(stdout, topic)
		} else {
			code, err = dispatch(a, stdin, stdout, system)
		}
	}
	if err == nil {
		return code
	}
	if _, writeErr := fmt.Fprintln(stderr, err); writeErr != nil {
		return 3
	}
	var pathError *os.PathError
	if errors.As(err, &pathError) || errors.Is(err, kvp.ErrInvalidFraming) {
		return 3
	}
	var usageErr *usageError
	var validationErr *kvp.ValidationError
	var decodeErr kvp.DecodeError
	if errors.As(err, &usageErr) || errors.As(err, &validationErr) || errors.As(err, &decodeErr) {
		return 2
	}
	for _, validation := range []error{kvp.ErrEmptyKey, kvp.ErrKeyContainsNull, kvp.ErrValueContainsNull,
		kvp.ErrInvalidUTF8, kvp.ErrEncoding, kvp.ErrPayloadNotUTF8, kvp.ErrInvalidDuration} {
		if errors.Is(err, validation) {
			return 2
		}
	}
	return 3
}

func help(out io.Writer, topic string) error {
	var text strings.Builder
	options := globalOptions
	if topic == "" {
		text.WriteString("Inspect and manipulate Linux Hyper-V KVP pool files.\n\nUsage: azure-init-kvp [OPTIONS] <COMMAND>\n\nCommands:\n")
		for _, command := range strings.Fields(commandOrder) {
			text.WriteString(fmt.Sprintf("  %-20s %s\n", command, commands[command].summary))
		}
	} else {
		spec := commands[topic]
		if topic == "help" {
			_, err := fmt.Fprintf(out, "%s\n\nUsage: azure-init-kvp help %s\n\nArguments:\n  [COMMAND]...  Print help for the subcommand(s)\n", spec.summary, spec.usage)
			return err
		}
		text.WriteString(fmt.Sprintf("%s\n\nUsage: azure-init-kvp %s [OPTIONS] %s\n", spec.summary, topic, spec.usage))
		options = strings.TrimSpace(spec.flags + " " + options)
	}
	text.WriteString("\nOptions (global options may appear before or after the command):\n")
	for _, name := range strings.Fields(options) {
		text.WriteString(fmt.Sprintf("  --%-20s %s\n", name, optionDescriptions[name]))
	}
	text.WriteString("\nDump defaults to JSON; other commands default to text.\n")
	_, err := io.WriteString(out, text.String())
	return err
}

func dispatch(a arguments, stdin io.Reader, out io.Writer, system services) (int, error) {
	pool, ok := poolArguments[a.value("pool", "guest")]
	if !ok {
		return 0, usage("invalid pool %q", a.value("pool", ""))
	}
	mode := kvp.Safe
	if a.bools["unsafe"] {
		mode = kvp.Unsafe
	}
	store, err := system.newStore(pool, a.value("dir", kvp.DefaultDir), mode)
	if err != nil {
		return 0, err
	}
	output := func(value interface{}, text string) error {
		if a.bools["json"] {
			return writeJSON(out, value)
		}
		_, err := fmt.Fprintln(out, text)
		return err
	}
	switch a.command {
	case "info":
		count, err := store.Len()
		if err != nil {
			return 0, err
		}
		stale, err := store.IsStale()
		if err != nil {
			return 0, err
		}
		value := map[string]interface{}{"pool": pool.String(), "path": store.Path(), "mode": mode.String(),
			"records": count, "empty": count == 0, "stale": stale, "max_key_size": store.MaxKeySize(), "max_value_size": store.MaxValueSize()}
		if a.bools["json"] {
			return 0, writeJSON(out, value)
		}
		for _, name := range []string{"pool", "path", "mode", "records", "empty", "stale", "max_key_size", "max_value_size"} {
			if _, err := fmt.Fprintf(out, "%s=%v\n", name, value[name]); err != nil {
				return 0, err
			}
		}
	case "dump":
		if a.bools["parse"] {
			return 0, dumpParsed(store, a, out)
		}
		records, err := store.Dump()
		if err != nil {
			return 0, err
		}
		if !a.bools["text"] {
			return 0, writeJSON(out, records)
		}
		for _, record := range records {
			if _, err := fmt.Fprintf(out, "%s=%s\n", record.Key, record.Value); err != nil {
				return 0, err
			}
		}
	case "entries":
		entries, err := store.Entries()
		if err != nil {
			return 0, err
		}
		if a.bools["json"] {
			return 0, writeJSON(out, entries)
		}
		keys := make([]string, 0, len(entries))
		for key := range entries {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if _, err := fmt.Fprintf(out, "%s=%s\n", key, entries[key]); err != nil {
				return 0, err
			}
		}
	case "read":
		value, found, err := store.Read(a.args[0])
		if err != nil {
			return 0, err
		}
		if !found {
			return 1, nil
		}
		return 0, output(kvp.Record{Key: a.args[0], Value: value}, value)
	case "write":
		if a.bools["append"] {
			return 0, store.Append(a.args[0], a.args[1])
		}
		return 0, store.Insert(a.args[0], a.args[1])
	case "load", "append-multiple":
		records, err := inputRecords(a, stdin)
		if err != nil {
			return 0, err
		}
		if a.command == "load" {
			return 0, store.Load(records)
		}
		return 0, store.AppendMultiple(records)
	case "delete":
		removed, err := store.Delete(a.args[0])
		if err != nil {
			return 0, err
		}
		return 0, output(map[string]bool{"removed": removed}, fmt.Sprint(removed))
	case "delete-multiple":
		removed, err := store.DeleteMultiple(a.args)
		if err != nil {
			return 0, err
		}
		return 0, output(map[string]int{"removed": removed}, fmt.Sprint(removed))
	case "clear":
		if a.bools["if-stale"] {
			return 0, store.ClearIfStale()
		}
		return 0, store.Clear()
	case "is-stale":
		stale, err := store.IsStale()
		if err != nil {
			return 0, err
		}
		err = output(map[string]bool{"stale": stale}, fmt.Sprint(stale))
		if !stale {
			return 1, err
		}
		return 0, err
	case "emit":
		vmID, err := resolveVM(a, system.vmID)
		if err != nil {
			return 0, err
		}
		return 0, store.emit(a.value("agent", defaultAgent), vmID, a.value("name", ""), a.value("message", ""))
	case "report-in-progress", "report-success", "report-failure":
		return 0, publishReport(store, a, system.vmID)
	default:
		return 0, usage("unsupported command %q", a.command)
	}
	return 0, nil
}

func writeJSON(out io.Writer, value interface{}) error {
	encoder := json.NewEncoder(out)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func inputRecords(a arguments, stdin io.Reader) ([]kvp.Record, error) {
	var data []byte
	var err error
	if paths := a.values["file"]; len(paths) != 0 {
		data, err = os.ReadFile(paths[0])
	} else {
		data, err = io.ReadAll(stdin)
	}
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(data) {
		return nil, &os.PathError{Op: "read", Path: a.value("file", "stdin"), Err: kvp.ErrInvalidUTF8}
	}
	records := make([]kvp.Record, 0)
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if i < len(lines)-1 {
			line = strings.TrimSuffix(line, "\r")
		}
		if line == "" {
			continue
		}
		position := strings.IndexByte(line, '=')
		if position < 0 {
			return nil, usage("line %d must be in key=value format", i+1)
		}
		records = append(records, kvp.Record{Key: line[:position], Value: line[position+1:]})
	}
	return records, nil
}

func resolveVM(a arguments, lookup func() (string, error)) (string, error) {
	if vmID := a.values["vm-id"]; len(vmID) != 0 {
		return vmID[0], nil
	}
	value, err := lookup()
	if err != nil {
		return "", usage("unable to determine current VM ID; pass --vm-id explicitly: %v", err)
	}
	return value, nil
}

func publishReport(store commandStore, a arguments, lookup func() (string, error)) error {
	extra := make([][2]string, 0)
	for _, value := range a.values["supporting-data"] {
		pairs, err := supportingData(value)
		if err != nil {
			return err
		}
		extra = append(extra, pairs...)
	}
	failure := a.command == "report-failure"
	hasURL := len(a.values["documentation-url"]) != 0
	if failure {
		for _, pair := range extra {
			if pair[0] == "reason" || (pair[0] == "documentation_url" && hasURL) {
				return usage("supporting data key %q duplicates a failure report field", pair[0])
			}
			hasURL = hasURL || pair[0] == "documentation_url"
		}
	}
	vmID, err := resolveVM(a, lookup)
	if err != nil {
		return err
	}
	agent := a.value("agent", defaultAgent)
	report := kvp.NewInProgressReport(agent, vmID, kvp.PPSNone)
	if a.command == "report-success" {
		report = kvp.NewSuccessReport(agent, vmID, kvp.PPSNone)
	} else if failure {
		report = kvp.NewFailureReport(agent, vmID, a.value("reason", ""), kvp.PPSNone)
		if has := a.values["documentation-url"]; len(has) != 0 {
			report = report.WithDocumentationURL(has[0])
		}
	}
	report.Extra = extra
	return store.publish(report)
}

func supportingData(raw string) ([][2]string, error) {
	pairs := make([][2]string, 0)
	for i := 0; i < len(raw); {
		start := i
		for i < len(raw) && raw[i] != '=' && raw[i] != ',' {
			i++
		}
		if i < len(raw) && raw[i] == '=' {
			i++
			if i < len(raw) && (raw[i] == '\'' || raw[i] == '"') {
				quote := raw[i]
				i++
				for i < len(raw) && raw[i] != quote {
					i++
				}
				if i == len(raw) {
					return nil, usage("supporting data has an unterminated quote")
				}
				i++
				if i < len(raw) && raw[i] != ',' {
					return nil, usage("unexpected characters after a quoted supporting value")
				}
			} else {
				for i < len(raw) && raw[i] != ',' {
					i++
				}
			}
		}
		field := raw[start:i]
		if field != "" {
			position := strings.IndexByte(field, '=')
			if position <= 0 {
				return nil, usage("supporting data %q must have a nonempty key and '='", field)
			}
			key, value := field[:position], field[position+1:]
			if containsWord("result agent pps_type vm_id timestamp", key) {
				return nil, usage("supporting data key %q is reserved for report fields", key)
			}
			if len(value) >= 2 && (value[0] == '\'' || value[0] == '"') && value[len(value)-1] == value[0] {
				value = value[1 : len(value)-1]
			}
			pairs = append(pairs, [2]string{key, value})
		}
		if i < len(raw) {
			i++
		}
	}
	return pairs, nil
}

func dumpParsed(store commandStore, a arguments, out io.Writer) error {
	records, err := store.Dump()
	if err != nil {
		return err
	}
	entries := kvp.DecodeEntries(records)
	filtered := make([]kvp.Entry, 0, len(entries))
	for _, entry := range entries {
		if d := entry.Diagnostic; d != nil {
			if !strings.Contains(d.Name, a.value("name", "")) || (a.value("kind", "") != "" && string(d.Kind) != a.value("kind", "")) {
				continue
			}
		}
		filtered = append(filtered, entry)
	}
	if !a.bools["text"] {
		return writeParsedEntries(out, filtered, false)
	}
	return writeParsedEntries(out, filtered, true)
}

func writeParsedEntries(out io.Writer, entries []kvp.Entry, text bool) error {
	if !text {
		return writeJSON(out, entries)
	}
	for _, entry := range entries {
		line, err := entryText(entry)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintln(out, line); err != nil {
			return err
		}
	}
	return nil
}

func entryText(entry kvp.Entry) (string, error) {
	var line string
	switch {
	case entry.Report != nil && entry.Raw == nil && entry.Diagnostic == nil:
		encoded, err := entry.Report.Encode()
		if err != nil {
			return "", err
		}
		line = kvp.ProvisioningReportKey + "=" + encoded
	case entry.Raw != nil && entry.Report == nil && entry.Diagnostic == nil:
		line = fmt.Sprintf("raw key=%s value=%s", entry.Raw.Key, entry.Raw.Value)
		if entry.Raw.Error != "" {
			line += " error=" + entry.Raw.Error.Error()
		}
	case entry.Diagnostic != nil && entry.Report == nil && entry.Raw == nil:
		d := entry.Diagnostic
		line = fmt.Sprintf("diagnostic kind=%s agent=%s", d.Kind, d.Agent)
		if d.VMID != nil {
			line += " vm_id=" + *d.VMID
		}
		line += fmt.Sprintf(" name=%s event_id=%s timestamp=%s encoding=%s", d.Name, d.EventID, d.TimestampString(), d.Encoding)
		if d.Result != nil {
			line += " result=" + string(*d.Result)
		}
		if d.Duration != nil {
			formatted, err := d.Duration.Format(kvp.Nanoseconds)
			if err != nil {
				return "", err
			}
			line += " duration=" + strings.TrimRight(strings.TrimRight(formatted, "0"), ".") + "s"
		}
		if text, ok := d.Payload.Text(); ok {
			line += " payload=" + text
		} else {
			line += " payload_b64=" + base64.StdEncoding.EncodeToString(d.Payload.Bytes())
		}
	default:
		return "", kvp.Malformed
	}
	return line, nil
}
