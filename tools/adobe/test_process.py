"""Synthetic cryptographic fixtures only; no real keys or books."""
import base64
import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import zipfile
import zlib

from Crypto.Cipher import AES, PKCS1_v1_5
from Crypto.PublicKey import RSA
from Crypto.Util.Padding import pad

spec = importlib.util.spec_from_file_location('adobe_process', Path(__file__).with_name('process.py'))
processor = importlib.util.module_from_spec(spec)
spec.loader.exec_module(processor)


class ImportTests(unittest.TestCase):
    def test_download_matches_upstream_headers_and_preserves_bytes(self):
        def response(request):
            self.assertEqual(request.get_header('User-agent'), 'book2png')
            self.assertEqual(request.get_header('Accept'), '*/*')
            return io.BytesIO(b'example book bytes')
        with tempfile.TemporaryDirectory() as temp, patch.object(processor, 'urlopen', side_effect=response):
            output = Path(temp) / 'download.partial'
            processor.download_book('https://example.invalid/book', output)
            self.assertEqual(output.read_bytes(), b'example book bytes')

    def test_download_limit(self):
        with tempfile.TemporaryDirectory() as temp, patch.object(processor, 'MAX_BYTES', 4), patch.object(processor, 'urlopen', return_value=io.BytesIO(b'12345')):
            with self.assertRaisesRegex(ValueError, 'download too large'):
                processor.download_book('https://example.invalid/book', Path(temp) / 'download.partial')

    def test_generated_adobe_epub_decrypt_and_resume(self):
        with tempfile.TemporaryDirectory() as temp:
            work = Path(temp)
            account = work / 'account'
            account.mkdir()
            (account / 'activation.xml').write_text('<activation xmlns="http://ns.adobe.com/adept"/>')
            key = RSA.generate(1024)
            wrong = RSA.generate(1024)
            (work / 'dedrm.json').write_text(json.dumps({'adeptkeys': {'wrong': wrong.export_key(format='DER').hex(), 'right': key.export_key(format='DER').hex()}}))
            aes = os.urandom(16)
            bookkey = base64.b64encode(PKCS1_v1_5.new(key.public_key()).encrypt(aes)).decode()
            rights = f'<rights xmlns="http://ns.adobe.com/adept"><licenseToken><encryptedKey>{bookkey}</encryptedKey></licenseToken></rights>'
            encryption = '<encryption xmlns="urn:oasis:names:tc:opendocument:xmlns:container"><EncryptedData xmlns="http://www.w3.org/2001/04/xmlenc#"><EncryptionMethod Algorithm="http://www.w3.org/2001/04/xmlenc#aes128-cbc"/><CipherData><CipherReference URI="chapter.xhtml"/></CipherData></EncryptedData></encryption>'
            content = b'<html xmlns="http://www.w3.org/1999/xhtml"><body>Example fixture</body></html>'
            compressor = zlib.compressobj(wbits=-15)
            compressed = compressor.compress(content) + compressor.flush()
            ciphertext = AES.new(aes, AES.MODE_CBC, bytes(16)).encrypt(pad(os.urandom(16) + compressed, 16))
            source = work / 'source.epub'
            with zipfile.ZipFile(source, 'w') as z:
                z.writestr('mimetype', 'application/epub+zip')
                z.writestr('META-INF/container.xml', '<container/>')
                z.writestr('META-INF/rights.xml', rights)
                z.writestr('META-INF/encryption.xml', encryption)
                z.writestr('chapter.xhtml', ciphertext)
            with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
                output = processor.process(source, work, work, account)
                self.assertEqual(processor.process(source, work, work, account), output)
            with zipfile.ZipFile(output) as z:
                self.assertEqual(z.read('chapter.xhtml'), content)
                self.assertNotIn('META-INF/rights.xml', z.namelist())

    def test_reject_private_network(self):
        for url in ('http://127.0.0.1/a', 'http://192.168.50.1/a', 'http://[::1]/a', 'file:///etc/passwd'):
            with self.assertRaises(ValueError):
                processor.public_url(url)

    def test_xml_entities_not_expanded(self):
        xml = b'<!DOCTYPE root [<!ENTITY secret SYSTEM "file:///etc/hostname">]><root>&secret;</root>'
        tree = processor.etree.fromstring(xml)
        self.assertIsNone(tree.text)


if __name__ == '__main__':
    unittest.main()
