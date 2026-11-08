import base64
import hashlib
import hmac
import json
import pathlib
import tempfile
import unittest

from verify_acquisition import verify


class VerificationTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = pathlib.Path(self.temp.name)
        self.key = b'x' * 32
        self.key_file = self.root / 'key'
        self.key_file.write_bytes(self.key)
        self.artifact = self.root / ('1' * 32)
        self.artifact.write_bytes(b'')  # Empty retirement evidence is valid.
        payload = json.dumps({'version': 1, 'deployment': 'trusted', 'r': 'capture',
                              'artifacts': [{'id': self.artifact.name, 'kind': 'evidence',
                                             'size': 0, 'sha256': hashlib.sha256(b'').hexdigest()}]}).encode()
        self.metadata = self.root / 'metadata'
        self.metadata.write_text(json.dumps({
            'payload': base64.b64encode(payload).decode(),
            'mac': hmac.new(self.key, b'TESSERA-ACQUISITION-1\n' + payload, hashlib.sha256).hexdigest()}))

    def test_empty_authenticated_evidence(self):
        verify(self.metadata, self.key_file, 'trusted')

    def test_wrong_deployment(self):
        with self.assertRaises(ValueError):
            verify(self.metadata, self.key_file, 'different')

    def test_replaced_artifact(self):
        self.artifact.write_bytes(b'tampered')
        with self.assertRaises(ValueError):
            verify(self.metadata, self.key_file, 'trusted')

    def test_replaced_metadata(self):
        envelope = json.loads(self.metadata.read_bytes())
        envelope['mac'] = '0' * 64
        self.metadata.write_text(json.dumps(envelope))
        with self.assertRaises(ValueError):
            verify(self.metadata, self.key_file, 'trusted')


if __name__ == '__main__':
    unittest.main()
