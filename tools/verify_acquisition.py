#!/usr/bin/env python3
"""Authenticate an acquisition and verify its sibling artifacts before restore.

Save artifacts using their opaque IDs as filenames, without renaming them. The
expected deployment comes from the previously trusted HTTPS acquisition response.
No key, path or retirement identity is printed.
"""
import argparse
import base64
import hashlib
import hmac
import json
import pathlib
import re
import sys


def verify(metadata, key_file, deployment):
    raw = metadata.read_bytes()
    if len(raw) > 1 << 20:
        raise ValueError()
    envelope = json.loads(raw)
    payload = base64.b64decode(envelope['payload'], validate=True)
    key = key_file.read_bytes()
    if len(key) != 32:
        raise ValueError()
    signature = hmac.new(key, b'TESSERA-ACQUISITION-1\n' + payload, hashlib.sha256).hexdigest()
    if not hmac.compare_digest(signature, envelope['mac']):
        raise ValueError()
    pair = json.loads(payload)
    if pair['version'] != 1 or pair['deployment'] != deployment or not pair['r']:
        raise ValueError()
    seen = set()
    for artifact in pair['artifacts']:
        name = artifact['id']
        if not re.fullmatch('[0-9a-f]{32}', name) or name in seen:
            raise ValueError()
        seen.add(name)
        source = metadata.parent / name
        digest = hashlib.sha256()
        size = 0
        with source.open('rb') as stream:
            while chunk := stream.read(1 << 20):
                size += len(chunk)
                if size > artifact['size']:
                    raise ValueError()
                digest.update(chunk)
        if size != artifact['size'] or not hmac.compare_digest(digest.hexdigest(), artifact['sha256']):
            raise ValueError()
    if not seen:
        raise ValueError()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--metadata', required=True, type=pathlib.Path)
    parser.add_argument('--key-file', required=True, type=pathlib.Path)
    parser.add_argument('--deployment', required=True)
    args = parser.parse_args()
    try:
        verify(args.metadata, args.key_file, args.deployment)
    except (OSError, ValueError, KeyError, TypeError):
        print('Acquisition verification failed.', file=sys.stderr)
        return 1
    print('Acquisition authenticated; artifact sizes and digests verified.')
    return 0


if __name__ == '__main__':
    sys.exit(main())
