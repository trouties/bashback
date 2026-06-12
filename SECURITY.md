# Security & secrets

bashback stores verbatim copies of your project files in shadow git repositories
under `~/.bashback`. This page states what that means for secrets.

## Storage is owner-only

`~/.bashback` and everything under it is created `0700`. It is not encrypted.
Anyone who can read your home directory can read the snapshots. Opt-in encryption
(age/git-crypt) is planned but **not yet implemented**.

## `.gitignore` is honored by default

The shadow repo respects your project `.gitignore`, so files like `.env` are
**not** snapshotted by default. The trade-off: a deleted `.gitignore`d `.env`
cannot be recovered. This is deliberate — secrets stay out of `~/.bashback`.

If you need to protect an ignored file, opt in explicitly with `bashback config
set force_include <path>` (bashback prints a copy-risk warning). **Doing so
stores a plaintext copy of that file under `~/.bashback`.** Only force-include
files you accept storing unencrypted.

## Command text is redacted before journaling

The journal records each command's text for audit. Before it is written,
bashback redacts common secret shapes — `Authorization`/`Bearer` headers,
`token=` / `password=` / `*_secret` / `*_api_key` assignments, AWS access key
ids — replacing the value with `***`, then truncates to 512 characters. The raw
command text is never written to disk.

Redaction rules cannot be exhaustive. Residual risk is carried by the `0700`
permissions and this disclosure. Do not rely on redaction as your only control
for secrets in command lines.

## The journal is append-only and permanent

The journal (`journal.jsonl`) is an audit ledger and is **never** deleted, even
after GC reclaims the underlying snapshots. It contains redacted command text,
timestamps, and snapshot hashes — but no file contents.

## Reporting

Report vulnerabilities through GitHub's **Private Vulnerability Reporting**:
open the repository's *Security* tab → *Report a vulnerability*. Reports go
privately to the maintainer; please do not open a public issue for security
problems. You should receive an initial response within a week.
