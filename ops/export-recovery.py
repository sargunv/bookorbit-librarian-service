#!/usr/bin/env python3
"""Export book-reference rows from a PostgreSQL custom-format backup, privately.

Uses the selected PostgreSQL container's existing POSTGRES_USER/POSTGRES_DB.
The database is read only; pg_restore emits selected table data to this process.
No catalog data or credentials are printed. Run on the container host.
"""

import argparse
import json
import os
from pathlib import Path
import re
import subprocess


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--container", required=True)
    parser.add_argument("--dump", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    if args.output.exists():
        raise SystemExit("Output already exists; preserve the previous recovery evidence")
    query = """
    SELECT jsonb_build_object(
      'refs',(SELECT jsonb_agg(x) FROM (
        SELECT DISTINCT rel.relname AS "table",a.attname AS "column"
        FROM pg_constraint c JOIN pg_class rel ON rel.oid=c.conrelid
        CROSS JOIN LATERAL unnest(c.conkey,c.confkey) k(local_num,target_num)
        JOIN pg_attribute a ON a.attrelid=c.conrelid AND a.attnum=k.local_num
        JOIN pg_attribute b ON b.attrelid=c.confrelid AND b.attnum=k.target_num
        WHERE c.contype='f' AND c.confrelid='public.books'::regclass AND b.attname='id'
      )x),
      'jsonColumns',(SELECT coalesce(jsonb_agg(table_name||'.'||column_name),'[]')
        FROM information_schema.columns WHERE table_schema='public' AND data_type IN ('json','jsonb')),
      'integerColumns',(SELECT coalesce(jsonb_agg(table_name||'.'||column_name),'[]')
        FROM information_schema.columns WHERE table_schema='public' AND data_type IN ('smallint','integer','bigint')));
    """
    metadata = json.loads(subprocess.check_output(
        ["podman", "exec", "-i", args.container, "sh", "-c",
         'psql -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -At'],
        input=query.encode()))
    refs = metadata["refs"]
    tables = sorted({ref["table"] for ref in refs})
    command = ["podman", "exec", "-i", args.container, "pg_restore", "--data-only", "--file=-"]
    command.extend("--table=" + table for table in tables)
    with args.dump.open("rb") as dump:
        result = subprocess.run(command, stdin=dump, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    if result.returncode:
        raise SystemExit("pg_restore could not extract backup data")
    escape = {"b": "\b", "t": "\t", "n": "\n", "r": "\r", "f": "\f", "v": "\v", "\\": "\\"}

    def unescape(value):
        return re.sub(r"\\([0-7]{1,3}|.)", lambda m: chr(int(m[1], 8))
                      if m[1][0].isdigit() else escape.get(m[1], m[1]), value)

    data = {table: [] for table in tables}
    table = None
    columns = []
    for line in result.stdout.decode().splitlines():
        match = re.match(r'COPY public\."?([a-z_]+)"? \((.*?)\) FROM stdin;', line)
        if match:
            table = match[1]
            columns = [c.strip('"') for c in match[2].split(", ")]
        elif line == "\\.":
            table = None
        elif table is not None:
            values = line.split("\t")
            if len(values) != len(columns):
                raise SystemExit("Unexpected COPY row shape")
            row = {}
            for col, raw in zip(columns, values):
                value = None if raw == r"\N" else unescape(raw)
                if value is not None and table + "." + col in metadata["jsonColumns"]:
                    value = json.loads(value)
                if value is not None and table + "." + col in metadata["integerColumns"]:
                    value = int(value)
                row[col] = value
            data[table].append(row)
    # Keep only actual references. Bibliographic snapshots are retained as
    # evidence, but the Go recovery command never reimports stale curation.
    snapshot = {"references": [dict(ref, rows=[row for row in data[ref["table"]]
                                              if row.get(ref["column"]) is not None]) for ref in refs]}
    args.output.parent.mkdir(parents=True, exist_ok=True)
    descriptor = os.open(args.output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(descriptor, "w") as output:
        json.dump(snapshot, output, ensure_ascii=False)
        output.flush()
        os.fsync(output.fileno())
    print(json.dumps({"exportedReferenceTables": len(refs)}))


if __name__ == "__main__":
    main()
