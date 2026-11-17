#!/usr/bin/env python3
"""Private, bounded Adobe import subprocess. Never emit upstream diagnostics."""
import base64
import contextlib
import importlib.util
import ipaddress
import json
import os
from pathlib import Path
import shutil
import socket
import ssl
import sys
import types
import urllib.parse
import urllib.request
import zipfile

from lxml import etree

ROOT = Path(__file__).resolve().parent
MAX_BYTES = 250 * 1024 * 1024
ADE = '{http://ns.adobe.com/adept}'
etree.set_default_parser(etree.XMLParser(resolve_entities=False, no_network=True))
socket.setdefaulttimeout(45)


def commit(temp, destination):
    with temp.open('rb') as file:
        os.fsync(file.fileno())
    os.replace(temp, destination)
    directory = os.open(destination.parent, os.O_RDONLY)
    try:
        os.fsync(directory)
    finally:
        os.close(directory)


def public_url(url):
    parsed = urllib.parse.urlsplit(url)
    if parsed.scheme not in ('http', 'https') or not parsed.hostname or parsed.username or parsed.password:
        raise ValueError('invalid remote URL')
    for addr in socket.getaddrinfo(parsed.hostname, parsed.port or (443 if parsed.scheme == 'https' else 80), type=socket.SOCK_STREAM):
        if not ipaddress.ip_address(addr[4][0]).is_global:
            raise ValueError('non-public remote URL')


class Redirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        public_url(newurl)
        return super().redirect_request(req, fp, code, msg, headers, newurl)


# Upstream disables certificate verification. Override its transport centrally.
# Every request and redirect is checked; XML entities and network DTDs are disabled.
def urlopen(req, data=None, timeout=45, **_kwargs):
    public_url(req.full_url if isinstance(req, urllib.request.Request) else req)
    ctx = ssl.create_default_context()
    ctx.maximum_version = ssl.TLSVersion.TLSv1_2
    return urllib.request.build_opener(urllib.request.ProxyHandler({}), Redirect(), urllib.request.HTTPSHandler(context=ctx)).open(req, data, timeout=timeout)


urllib.request.urlopen = urlopen


def load_dedrm():
    folder = ROOT / 'vendor/dedrm'
    sys.path.insert(0, str(folder))
    package = types.ModuleType('adobe_dedrm')
    package.__path__ = [str(folder)]
    sys.modules['adobe_dedrm'] = package
    loaded = []
    for name in ('ineptepub', 'ineptpdf'):
        spec = importlib.util.spec_from_file_location('adobe_dedrm.' + name, folder / (name + '.py'))
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        loaded.append(module)
    return loaded


def validate(path, ext):
    if not path.is_file() or not 0 < path.stat().st_size <= MAX_BYTES:
        raise ValueError('invalid output size')
    if ext == '.epub':
        with zipfile.ZipFile(path) as z:
            if sum(i.file_size for i in z.infolist()) > 1024 * 1024 * 1024:
                raise ValueError('expanded EPUB too large')
            if z.read('mimetype') != b'application/epub+zip' or 'META-INF/container.xml' not in z.namelist():
                raise ValueError('invalid EPUB')
            if 'META-INF/encryption.xml' in z.namelist():
                encryption = etree.fromstring(z.read('META-INF/encryption.xml'))
                algorithms = encryption.xpath('//*[local-name()="EncryptionMethod"]/@Algorithm')
                if any(a not in ('http://www.idpf.org/2008/embedding', 'http://ns.adobe.com/pdf/enc#RC') for a in algorithms):
                    raise ValueError('content encryption remains')
            if 'META-INF/rights.xml' in z.namelist():
                raise ValueError('Adobe DRM remains')
            if z.testzip() is not None:
                raise ValueError('invalid EPUB CRC')
    elif ext == '.pdf':
        import pikepdf
        with pikepdf.open(path) as pdf:
            if pdf.is_encrypted or len(pdf.pages) == 0:
                raise ValueError('encrypted or empty PDF')
    else:
        raise ValueError('unsupported format')


def fulfill(source, work, account):
    sys.path.insert(0, str(ROOT / 'vendor/deacsm'))
    import libadobe
    import libadobeFulfill
    from libpdf import patch_drm_into_pdf
    libadobe.update_account_path(str(account))
    replyfile = work / 'fulfillment.xml'
    if replyfile.exists():
        reply = replyfile.read_bytes()
    else:
        success, reply = libadobeFulfill.fulfill(str(source), do_notify=True)
        if not success:
            raise ValueError('fulfillment failed')
        reply = reply.encode() if isinstance(reply, str) else reply
        temporary = work / 'fulfillment.partial'
        temporary.write_bytes(reply)
        commit(temporary, replyfile)
    tree = etree.fromstring(reply)
    info = tree.find('.//' + ADE + 'resourceItemInfo')
    token = info.find(ADE + 'licenseToken')
    rights = libadobeFulfill.buildRights(token)
    if not rights:
        raise ValueError('missing rights')
    download = work / 'download.bin'
    if not download.exists():
        temp = work / 'download.partial'
        with urlopen(info.findtext(ADE + 'src')) as response, temp.open('wb') as output:
            size = 0
            while chunk := response.read(65536):
                size += len(chunk)
                if size > MAX_BYTES:
                    raise ValueError('download too large')
                output.write(chunk)
        commit(temp, download)
    signature = download.open('rb').read(5)
    if signature.startswith(b'PK'):
        result = work / 'encrypted.epub'
        if not result.exists():
            temp = work / 'encrypted.partial'
            shutil.copyfile(download, temp)
            with zipfile.ZipFile(temp, 'a') as z:
                if 'META-INF/rights.xml' not in z.namelist():
                    z.writestr('META-INF/rights.xml', rights)
            commit(temp, result)
    elif signature.startswith(b'%PDF'):
        result = work / 'encrypted.pdf'
        if not result.exists():
            temp = work / 'encrypted.partial'
            if not patch_drm_into_pdf(str(download), rights, str(temp), token.findtext(ADE + 'resource')):
                raise ValueError('PDF rights failed')
            commit(temp, result)
    else:
        raise ValueError('unsupported fulfilled content')
    return result


def process(source, work, secrets, account):
    epub, pdf = load_dedrm()
    if source.suffix == '.acsm':
        source = fulfill(source, work, account)
    ext = source.suffix
    output = work / ('book' + ext)
    if output.exists():
        validate(output, ext)
        return output
    temp = work / ('decrypted' + ext)
    try:
        validate(source, ext)
        shutil.copyfile(source, temp)
    except Exception:
        config = json.loads((secrets / 'dedrm.json').read_text())
        keys = [bytes.fromhex(value) for value in config.get('adeptkeys', {}).values()]
        activation = etree.parse(str(account / 'activation.xml'))
        private = activation.findtext('.//' + ADE + 'privateLicenseKey')
        if private:
            keys.append(base64.b64decode(private)[26:])
        for key in keys:
            try:
                result = (epub if ext == '.epub' else pdf).decryptBook(key, str(source), str(temp))
                if result == 0:
                    validate(temp, ext)
                    break
            except Exception:
                pass
        else:
            raise ValueError('no matching Adobe key or unsupported content')
    validate(temp, ext)
    commit(temp, output)
    return output


if __name__ == '__main__':
    os.umask(0o077)
    try:
        # Suppress upstream output, including XML, credentials and signed URLs.
        with open(os.devnull, 'w') as quiet, contextlib.redirect_stdout(quiet), contextlib.redirect_stderr(quiet):
            output = process(*(Path(value).resolve() for value in sys.argv[1:5]))
        print(json.dumps({'filename': output.name}))
    except Exception:
        print('Adobe processing failed; check file format, matching ADE key, activation, or distributor availability.', file=sys.stderr)
        sys.exit(1)
