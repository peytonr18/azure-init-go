// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kvp

import (
	"strings"
	"time"
	"unicode/utf8"
)

const ProvisioningReportKey = "PROVISIONING_REPORT"

type ReportResult string

const (
	ResultInProgress ReportResult = "in_progress"
	ResultSuccess    ReportResult = "success"
	ResultError      ReportResult = "error"
)

type ReportPPSType string

const (
	PPSNone    ReportPPSType = "None"
	PPSOSDisk  ReportPPSType = "PreprovisionedOSDisk"
	PPSRunning ReportPPSType = "Running"
	PPSSavable ReportPPSType = "Savable"
	PPSUnknown ReportPPSType = "Unknown"
)

// ProvisioningReport is a single replaceable health report, not a diagnostic
// history. Timestamp spellings and ordered, possibly duplicate extras survive
// parsing. Reports do not impose diagnostic UUID or identity-length rules.
type ProvisioningReport struct {
	Result           ReportResult  `json:"result"`
	Agent            string        `json:"agent"`
	VMID             string        `json:"vm_id"`
	Timestamp        string        `json:"timestamp"`
	PPSType          ReportPPSType `json:"pps_type"`
	Reason           *string       `json:"reason,omitempty"`
	DocumentationURL *string       `json:"documentation_url,omitempty"`
	Extra            [][2]string   `json:"extra,omitempty"`
}

func NewInProgressReport(agent, vmID string, pps ReportPPSType) ProvisioningReport {
	return newReport(ResultInProgress, agent, vmID, pps)
}

func NewSuccessReport(agent, vmID string, pps ReportPPSType) ProvisioningReport {
	return newReport(ResultSuccess, agent, vmID, pps)
}

func NewFailureReport(agent, vmID, reason string, pps ReportPPSType) ProvisioningReport {
	report := newReport(ResultError, agent, vmID, pps)
	report.Reason = &reason
	return report
}

func newReport(result ReportResult, agent, vmID string, pps ReportPPSType) ProvisioningReport {
	return ProvisioningReport{
		Result: result, Agent: agent, VMID: vmID, PPSType: pps,
		Timestamp: formatTimestamp(time.Now().UTC(), -1, false),
	}
}

func (r ProvisioningReport) WithExtra(key, value string) ProvisioningReport {
	extra := make([][2]string, len(r.Extra), len(r.Extra)+1)
	copy(extra, r.Extra)
	r.Extra = append(extra, [2]string{key, value})
	return r
}

// WithDocumentationURL sets a URL that is encoded only for failure reports.
func (r ProvisioningReport) WithDocumentationURL(url string) ProvisioningReport {
	r.DocumentationURL = &url
	return r
}

// Encode produces pipe-delimited CSV, not a split-on-pipe string. It rejects
// invalid Go enum values and invalid UTF-8, which Rust's types cannot represent.
// Storage limits and NUL checks belong to WriteReport and the configured store.
func (r ProvisioningReport) Encode() (string, error) {
	if !validPPS(r.PPSType) {
		return "", Malformed
	}
	data := []string{"result=" + string(r.Result)}
	extras := func() {
		for _, pair := range r.Extra {
			data = append(data, pair[0]+"="+pair[1])
		}
	}
	switch r.Result {
	case ResultInProgress, ResultSuccess:
		data = append(data, "agent="+r.Agent, "pps_type="+string(r.PPSType),
			"vm_id="+r.VMID, "timestamp="+r.Timestamp)
		extras()
	case ResultError:
		if r.Reason == nil {
			return "", Malformed
		}
		data = append(data, "reason="+*r.Reason, "agent="+r.Agent)
		extras()
		data = append(data, "pps_type="+string(r.PPSType), "vm_id="+r.VMID, "timestamp="+r.Timestamp)
		if r.DocumentationURL != nil {
			data = append(data, "documentation_url="+*r.DocumentationURL)
		}
	default:
		return "", Malformed
	}
	for i, field := range data {
		if !utf8.ValidString(field) {
			return "", ErrInvalidUTF8
		}
		if strings.ContainsAny(field, "|\"\r\n") {
			data[i] = `"` + strings.ReplaceAll(field, `"`, `""`) + `"`
		}
	}
	return strings.Join(data, "|"), nil
}

// ParseProvisioningReport preserves the original timestamp and supporting data.
// One trailing LF or CRLF is accepted; unquoted newlines and broken CSV are not.
func ParseProvisioningReport(value string) (ProvisioningReport, error) {
	var report ProvisioningReport
	if !utf8.ValidString(value) {
		return report, Malformed
	}
	if strings.HasSuffix(value, "\r\n") {
		value = strings.TrimSuffix(value, "\r\n")
	} else {
		value = strings.TrimSuffix(value, "\n")
	}
	fields, err := parseReportCSV(value)
	if err != nil {
		return report, err
	}
	pairs := make([][2]string, 0, len(fields))
	for _, field := range fields {
		position := strings.IndexByte(field, '=')
		if position <= 0 {
			return report, Malformed
		}
		pairs = append(pairs, [2]string{field[:position], field[position+1:]})
	}
	take := func(name string, required bool) (string, bool, error) {
		position := -1
		for i, pair := range pairs {
			if pair[0] == name {
				if position >= 0 {
					return "", false, Malformed
				}
				position = i
			}
		}
		if position < 0 {
			if required {
				return "", false, Malformed
			}
			return "", false, nil
		}
		value := pairs[position][1]
		pairs = append(pairs[:position], pairs[position+1:]...)
		return value, true, nil
	}
	result, _, err := take("result", true)
	if err != nil {
		return report, err
	}
	report.Result = ReportResult(result)
	if report.Result != ResultInProgress && report.Result != ResultSuccess && report.Result != ResultError {
		return report, Malformed
	}
	for _, field := range []struct {
		name string
		dest *string
	}{{"agent", &report.Agent}, {"vm_id", &report.VMID}, {"timestamp", &report.Timestamp}} {
		*field.dest, _, err = take(field.name, true)
		if err != nil {
			return report, err
		}
	}
	if _, err := parseTimestamp(report.Timestamp); err != nil {
		return report, Malformed
	}
	pps, _, err := take("pps_type", true)
	if err != nil || !validPPS(ReportPPSType(pps)) {
		return report, Malformed
	}
	report.PPSType = ReportPPSType(pps)
	if report.Result == ResultError {
		reason, _, err := take("reason", true)
		if err != nil {
			return report, err
		}
		report.Reason = &reason
		url, found, err := take("documentation_url", false)
		if err != nil {
			return report, err
		}
		if found {
			report.DocumentationURL = &url
		}
	}
	report.Extra = pairs
	return report, nil
}

func validPPS(pps ReportPPSType) bool {
	switch pps {
	case PPSNone, PPSOSDisk, PPSRunning, PPSSavable, PPSUnknown:
		return true
	default:
		return false
	}
}

func parseReportCSV(value string) ([]string, error) {
	const (
		start = iota
		unquoted
		quoted
		closed
	)
	state := start
	fields := make([]string, 0)
	var field strings.Builder
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c == 0 {
			return nil, Malformed
		}
		switch state {
		case quoted:
			if c == '"' {
				state = closed
			} else {
				field.WriteByte(c)
			}
		case closed:
			switch c {
			case '"':
				field.WriteByte('"')
				state = quoted
			case '|':
				fields = append(fields, field.String())
				field.Reset()
				state = start
			default:
				return nil, Malformed
			}
		default:
			switch c {
			case '"':
				if state != start {
					return nil, Malformed
				}
				state = quoted
			case '|':
				fields = append(fields, field.String())
				field.Reset()
				state = start
			case '\r', '\n':
				return nil, Malformed
			default:
				field.WriteByte(c)
				state = unquoted
			}
		}
	}
	if state == quoted {
		return nil, Malformed
	}
	return append(fields, field.String()), nil
}

// WriteReport replaces the report key. Reports must fit in one store value.
// It performs no HTTP reporting.
func WriteReport(store *Store, report ProvisioningReport) error {
	value, err := report.Encode()
	if err != nil {
		return err
	}
	return store.Insert(ProvisioningReportKey, value)
}

// ReportInProgress reserves or replaces the report key before diagnostics can
// exhaust the unique-key budget. It never clears diagnostics.
func ReportInProgress(store *Store, agent, vmID string, pps ReportPPSType) error {
	return WriteReport(store, NewInProgressReport(agent, vmID, pps))
}

func ReportSuccess(store *Store, agent, vmID string, pps ReportPPSType) error {
	return WriteReport(store, NewSuccessReport(agent, vmID, pps))
}

func ReportFailure(store *Store, agent, vmID, reason string, pps ReportPPSType) error {
	return WriteReport(store, NewFailureReport(agent, vmID, reason, pps))
}
