# Adobe import components

The Go service invokes these components in a separate Python process. No Calibre
installation or plugin entry point is loaded. Vendored files are unmodified.

- `vendor/dedrm`: selected noDRM/DeDRM_tools modules at
  `7379b453199ed1ba91bf3a4ce4875d5ed3c309a9`, GPL-3.0, retaining file notices.
  Source: https://github.com/noDRM/DeDRM_tools
- `vendor/deacsm`: selected Leseratte10/acsm-calibre-plugin modules at
  `4eff3ee35ac760ccad063639401b303b7207aa9c`, GPL-3.0; license included. Source:
  https://github.com/Leseratte10/acsm-calibre-plugin
- Build installs DeACSM's published oscrypto 1.3.0 OpenSSL 3 compatibility fork,
  verified by SHA-256. This follows upstream `package_modules.sh`.

The wrapper enforces certificate verification, public HTTP(S) destinations,
request timeouts, download limits and safe XML parsing. Upstream diagnostics are
suppressed because they may contain activation credentials and signed URLs.
