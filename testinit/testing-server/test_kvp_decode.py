# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import csv
import io
import unittest

import kvp_decode
from module_provisioning import encode_record


class IndependentDecoderTests(unittest.TestCase):
    def test_binary_records_and_csv_context_are_preserved(self):
        reason = 'failed | "quoted"\r\nsecond line'
        output = io.StringIO(newline="")
        csv.writer(output, delimiter="|", lineterminator="").writerow(
            ["result=error", "agent=agent", "vm_id=vm", "timestamp=2026-01-01T00:00:00Z",
             "pps_type=None", "reason=" + reason, "extra=one", "extra=two"]
        )
        encoded = output.getvalue()
        rows = kvp_decode.records(encode_record("PROVISIONING_REPORT", encoded))
        report = kvp_decode.report(rows)
        self.assertEqual(report["encoded"], encoded)
        self.assertIn(("reason", reason), report["pairs"])
        self.assertEqual([v for k, v in report["pairs"] if k == "extra"], ["one", "two"])

    def test_corruption_and_ambiguous_reports_fail(self):
        with self.assertRaises(ValueError):
            kvp_decode.records(b"truncated")
        for rows in (
            [("PROVISIONING_REPORT", "missing-fields")],
            [("PROVISIONING_REPORT", "x"), ("PROVISIONING_REPORT", "y")],
        ):
            with self.assertRaises(ValueError):
                kvp_decode.report(rows)
