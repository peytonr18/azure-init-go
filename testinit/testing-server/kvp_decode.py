# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Independent pool/report/diagnostic decoding for the testinit assertions."""

import base64
import csv
import hashlib
import io
import zlib


def records(data):
    if len(data) % 2560:
        raise ValueError("pool size is not a multiple of 2560 bytes")
    result = []
    for offset in range(0, len(data), 2560):
        record = data[offset : offset + 2560]
        key = record[:512].split(b"\0", 1)[0].decode("utf-8")
        value = record[512:].split(b"\0", 1)[0].decode("utf-8")
        result.append((key, value))
    return result


def report(pool):
    values = [value for key, value in pool if key == "PROVISIONING_REPORT"]
    if not values:
        return None
    if len(values) != 1:
        raise ValueError("expected exactly one replaceable provisioning report")
    rows = list(csv.reader(io.StringIO(values[0], newline=""), delimiter="|", strict=True))
    if len(rows) != 1:
        raise ValueError("report must be one CSV record")
    pairs = []
    for field in rows[0]:
        if "=" not in field:
            raise ValueError("report field has no '='")
        key, value = field.split("=", 1)
        if not key:
            raise ValueError("report field has an empty name")
        pairs.append((key, value))
    standard = {}
    for key in ("result", "agent", "vm_id", "timestamp", "pps_type"):
        matches = [value for name, value in pairs if name == key]
        if len(matches) != 1:
            raise ValueError(f"ambiguous/missing report field {key}")
        standard[key] = matches[0]
    standard["pairs"] = pairs
    standard["encoded"] = values[0]
    return standard


def diagnostics(pool):
    groups = {}
    for key, value in pool:
        if not key.startswith("DIAG|"):
            continue
        fields = key.split("|")
        if len(fields) != 11 or len(key.encode()) > 254 or len(value.encode()) > 1022:
            raise ValueError("invalid emitted diagnostic framing")
        base = tuple(fields[:-1])
        groups.setdefault(base, []).append((int(fields[-1]), value))
    decoded = []
    for fields, chunks in groups.items():
        chunks.sort()
        if [index for index, _ in chunks] != list(range(len(chunks))):
            raise ValueError("missing/duplicate diagnostic chunks")
        value = "".join(fragment for _, fragment in chunks)
        encoding = fields[7]
        if encoding == "none":
            payload = value.encode("utf-8")
        elif encoding in ("zlib+b64", "gz+b64"):
            compressed = base64.b64decode(value, validate=True)
            decoder = zlib.decompressobj(15 if encoding == "zlib+b64" else 31)
            payload = decoder.decompress(compressed) + decoder.flush()
            if not decoder.eof or decoder.unused_data or decoder.unconsumed_tail:
                raise ValueError("incomplete or concatenated compressed stream")
        else:
            raise ValueError(f"unexpected writer encoding {encoding}")
        decoded.append(
            {
                "agent": fields[1],
                "vm_id": fields[2],
                "kind": fields[3],
                "name": fields[4],
                "event_id": fields[5],
                "sha256": hashlib.sha256(payload).hexdigest(),
                "chunks": len(chunks),
            }
        )
    return decoded
