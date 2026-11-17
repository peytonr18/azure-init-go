// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kvp

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

const (
	testVM        = "00000000-0000-0000-0000-000000000abc"
	testAgent     = "Azure-Init/0.0.0"
	testTimestamp = "2026-06-17T00:00:00+00:00"
)

func TestReportWireLayouts(t *testing.T) {
	for _, pps := range []ReportPPSType{PPSNone, PPSOSDisk, PPSRunning, PPSSavable, PPSUnknown} {
		for _, result := range []ReportResult{ResultInProgress, ResultSuccess, ResultError} {
			report := NewSuccessReport(testAgent, testVM, pps)
			report.Result = result
			report.Timestamp = testTimestamp
			report = report.WithExtra("endpoint", "http://example.com").WithExtra("endpoint", "again")
			reason := "failure"
			if result == ResultError {
				report.Reason = &reason
				report = report.WithDocumentationURL("http://help")
			}
			value, err := report.Encode()
			requireNoError(t, err)
			middle := "agent=" + testAgent + "|pps_type=" + string(pps) + "|vm_id=" + testVM + "|timestamp=" + testTimestamp
			want := "result=" + string(result) + "|" + middle + "|endpoint=http://example.com|endpoint=again"
			if result == ResultError {
				want = "result=error|reason=failure|agent=" + testAgent + "|endpoint=http://example.com|endpoint=again|pps_type=" + string(pps) + "|vm_id=" + testVM + "|timestamp=" + testTimestamp + "|documentation_url=http://help"
			}
			if value != want {
				t.Fatalf("encoded %q, want %q", value, want)
			}
			decoded, err := ParseProvisioningReport(value)
			requireNoError(t, err)
			if !reflect.DeepEqual(decoded, report) {
				t.Fatalf("round trip: %#v != %#v", decoded, report)
			}
		}
	}
}

func TestReportCSVAndPreservedIdentity(t *testing.T) {
	report := NewFailureReport("unrestricted agent", "not a UUID", "pipe|quote\"\r\nnext", PPSNone).
		WithExtra("dupe", "one=two").
		WithExtra("dupe", "three|four").
		WithDocumentationURL("https://help/?a=b|c")
	report.Timestamp = "2026-06-17T04:30:00.123400+04:30"
	value, err := report.Encode()
	requireNoError(t, err)
	if !strings.Contains(value, `"reason=pipe|quote""`+"\r\n"+`next"`) {
		t.Fatal(value)
	}
	for _, suffix := range []string{"", "\n", "\r\n"} {
		decoded, err := ParseProvisioningReport(value + suffix)
		requireNoError(t, err)
		if !reflect.DeepEqual(report, decoded) {
			t.Fatalf("report changed: %#v", decoded)
		}
	}
	success := NewSuccessReport("", "", PPSNone).WithDocumentationURL("ignored")
	success.Timestamp = testTimestamp
	encoded, err := success.Encode()
	requireNoError(t, err)
	if strings.Contains(encoded, "documentation_url") {
		t.Fatal(encoded)
	}
	parsed, err := ParseProvisioningReport(encoded + "|reason=ordinary|reason=duplicate")
	requireNoError(t, err)
	if len(parsed.Extra) != 2 || parsed.Reason != nil {
		t.Fatal(parsed)
	}
}

func TestReportMalformedCases(t *testing.T) {
	base := "result=success|agent=a|pps_type=None|vm_id=v|timestamp=" + testTimestamp
	for _, value := range []string{
		"", "novalue", base + "|", base + "|=value", base + "|agent=again",
		base + "|result=success", base + "|vm_id=again", base + "|timestamp=" + testTimestamp,
		strings.Replace(base, "pps_type=None", "pps_type=invalid", 1),
		strings.Replace(base, "result=success", "result=invalid", 1),
		strings.Replace(base, testTimestamp, "yesterday", 1),
		strings.Replace(base, "|agent=a", "", 1),
		strings.Replace(base, "result=success", "result=error", 1),
		base + "|a=bare\"quote", base + "|\"unterminated", base + "|\"a=b\"suffix",
		base + "|a=line\nunquoted", base + "|a=null\x00byte", base + "\n\n",
		strings.Replace(base, "result=success", "result=error|reason=x|reason=y", 1),
		strings.Replace(base, "result=success", "result=error|reason=x|documentation_url=a|documentation_url=b", 1),
	} {
		if _, err := ParseProvisioningReport(value); !errors.Is(err, Malformed) {
			t.Fatalf("accepted malformed report %q: %v", value, err)
		}
	}
}

func TestHealthHelpersReplaceOnlyReport(t *testing.T) {
	store := testStore(t, Safe)
	requireNoError(t, store.Append("diagnostic", "keep"))
	for _, write := range []func() error{
		func() error { return ReportInProgress(store, testAgent, testVM, PPSNone) },
		func() error { return ReportFailure(store, testAgent, testVM, "failed", PPSRunning) },
		func() error { return ReportSuccess(store, testAgent, testVM, PPSNone) },
		func() error { return ReportInProgress(store, testAgent, testVM, PPSNone) },
	} {
		requireNoError(t, write())
		records, err := store.Dump()
		requireNoError(t, err)
		if len(records) != 2 || records[0] != (Record{"diagnostic", "keep"}) {
			t.Fatal(records)
		}
		report, err := ParseProvisioningReport(records[1].Value)
		requireNoError(t, err)
		if report.Timestamp == "" {
			t.Fatal("missing timestamp")
		}
	}
	before, err := store.Dump()
	requireNoError(t, err)
	for _, agent := range []string{"a\x00b", strings.Repeat("a", SafeMaxValueBytes)} {
		if err := ReportSuccess(store, agent, testVM, PPSNone); err == nil {
			t.Fatal("expected validation failure")
		}
		recordsEqual(t, store, before)
	}
}

func FuzzProvisioningReport(f *testing.F) {
	for _, value := range []string{
		"result=success|agent=a|pps_type=None|vm_id=v|timestamp=" + testTimestamp,
		`"unterminated`, "", "result=error|reason=\"|", "\x00",
	} {
		f.Add(value)
	}
	f.Fuzz(func(t *testing.T, value string) {
		report, err := ParseProvisioningReport(value)
		if err != nil {
			return
		}
		encoded, err := report.Encode()
		requireNoError(t, err)
		again, err := ParseProvisioningReport(encoded)
		requireNoError(t, err)
		if !reflect.DeepEqual(report, again) {
			t.Fatalf("unstable report: %#v != %#v", report, again)
		}
	})
}
