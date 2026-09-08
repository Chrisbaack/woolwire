#!/usr/bin/env python3
"""Check repository doc links and explicit Go routes against our OpenAPI inventory.

Uses only Python's standard library. This checks the repository's flat YAML
layout and endpoint parity; use an OpenAPI 3.1 validator for full schema validation.
Run with --write to regenerate the marked inventory section in docs/API.md.
"""
from pathlib import Path
import argparse
import re
import subprocess
import sys
from urllib.parse import unquote, urlsplit

ROOT = Path(__file__).resolve().parents[1]
ROUTERS = (
    'internal/localapi/server.go',
    'internal/peerapi/server.go',
    'internal/runner/controller.go',
)
START = '<!-- endpoint-inventory:start -->'
END = '<!-- endpoint-inventory:end -->'


def operations():
    """Read path, method, summary from the checked-in OpenAPI layout."""
    result = {}
    path = method = None
    for line in (ROOT / 'docs/openapi.yaml').read_text().splitlines():
        match = re.fullmatch(r'  (/\S+):', line)
        if match:
            path, method = match[1], None
        match = re.fullmatch(r'    (get|post|put|patch|delete|head|options):', line)
        if match and path:
            method = match[1].upper()
            key = (method, path)
            if key in result:
                raise ValueError(f'Duplicate OpenAPI operation: {key}')
            result[key] = ''
        if line.startswith('      summary: ') and path and method:
            result[(method, path)] = line.removeprefix('      summary: ').strip()
    return result


def registered_routes():
    result = set()
    for filename in ROUTERS:
        source = (ROOT / filename).read_text()
        for method, path in re.findall(r'\.Handle(?:Func)?\("(GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS) (/[^"\s]+)"', source):
            # Go trailing wildcards are represented by ordinary path parameters
            # in OpenAPI; the parameter description explains segment escaping.
            path = re.sub(r'\{(\w+)\.\.\.\}', r'{\1}', path)
            result.add((method, path))
    return result


def inventory(ops):
    sections = [
        ('Owner API', '/api/v1/'),
        ('Compatible client API', '/v1/'),
        ('Bootstrap API', '/bootstrap/v1/'),
        ('Peer API', '/peer/v1/'),
        ('Runner API', '/runner/v1/'),
    ]
    lines = [START]
    for title, prefix in sections:
        lines += ['', f'### {title}', '', '| Method | Path | Purpose |', '|---|---|---|']
        for (method, path), summary in ops.items():
            if path.startswith(prefix):
                lines.append(f'| `{method}` | `{path}` | {summary} |')
    return '\n'.join(lines) + '\n\n' + END


def without_fences(text):
    lines, fence = [], None
    for line in text.splitlines():
        marker = re.match(r'^\s*(`{3,}|~{3,})', line)
        if marker:
            if fence is None:
                fence = marker[1][0]
            elif marker[1][0] == fence:
                fence = None
            continue
        if fence is None:
            lines.append(line)
    return '\n'.join(lines)


def anchors(path):
    found, counts = set(), {}
    for heading in re.findall(r'^#{1,6}\s+(.+?)\s*#*$', without_fences(path.read_text()), re.M):
        slug = re.sub(r'[^\w\-\s]', '', heading.lower()).replace(' ', '-')
        count = counts.get(slug, 0)
        found.add(f'{slug}-{count}' if count else slug)
        counts[slug] = count + 1
    return found


def check_links():
    errors = []
    names = subprocess.check_output(
        ['git', 'ls-files', '-z', '--cached', '--others', '--exclude-standard'], cwd=ROOT
    ).decode().split('\0')
    docs = sorted({ROOT / name for name in names if name.endswith('.md') and (ROOT / name).is_file()})
    for doc in docs:
        for target in re.findall(r'!?\[[^\]\n]*\]\(([^\s)]+)(?:\s+"[^"]*")?\)', without_fences(doc.read_text())):
            url = urlsplit(target.strip('<>'))
            if url.scheme in ('http', 'https', 'mailto'):
                continue
            if url.scheme or url.path.startswith('/'):
                errors.append(f'{doc.relative_to(ROOT)}: nonportable link {target}')
                continue
            destination = (doc.parent / unquote(url.path)).resolve() if url.path else doc
            if not destination.exists():
                errors.append(f'{doc.relative_to(ROOT)}: missing target {target}')
            elif destination.suffix == '.md' and url.fragment and unquote(url.fragment) not in anchors(destination):
                errors.append(f'{doc.relative_to(ROOT)}: missing heading {target}')
    return errors, len(docs)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--write', action='store_true', help='Regenerate endpoint inventory')
    args = parser.parse_args()
    ops = operations()
    routes = registered_routes()
    errors = []
    for method, path in sorted(routes - ops.keys()):
        errors.append(f'Missing OpenAPI operation: {method} {path}')
    for method, path in sorted(ops.keys() - routes):
        errors.append(f'OpenAPI operation has no explicit Go route: {method} {path}')
    for key, summary in ops.items():
        if not summary:
            errors.append(f'Operation has no summary: {key}')
    api = ROOT / 'docs/API.md'
    text = api.read_text()
    if text.count(START) != 1 or text.count(END) != 1:
        errors.append('API.md must contain exactly one inventory marker pair')
    else:
        expected = text[:text.index(START)] + inventory(ops) + text[text.index(END) + len(END):]
        if args.write:
            api.write_text(expected)
        elif text != expected:
            errors.append('Endpoint inventory is stale: run python3 scripts/check_docs.py --write')
    link_errors, count = check_links()
    errors.extend(link_errors)
    if errors:
        print('\n'.join(errors), file=sys.stderr)
        return 1
    print(f'Checked {count} Markdown files and {len(ops)} API operations: links and route inventory agree.')
    return 0


if __name__ == '__main__':
    sys.exit(main())
