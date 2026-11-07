---
name: project
description: Maintain CPA Model Router within its existing host, routing, and storage contracts.
---

CPA Model Router is a Go native c-shared CLIProxyAPI plugin. Use Go 1.26.0,
make, and a working C compiler; SQLite and the native ABI require CGO.
Read README.md, Makefile, and the affected implementation and tests before editing.

CPA owns provider credentials, protocol translation, proxy policy, and execution.
Preserve minimum host compatibility with CPA v7.3.9. Keep routing failover,
stream-start boundaries, model attribution, and durable SQLite history intact.
Add a regression that exercises the real logic behind the evidenced bug.
Keep edits within the supervisor's frozen bug objective.

Run make check and make build for local validation. The test CI job runs the
black-box CPA integration against its pinned minimum-supported source. Native
Windows SQLite and CodeQL checks are remote gates; report them as pending until
their results exist. Never represent a Linux check as Windows coverage.

For workflow changes, run npm ci, npm run test:orm, and actionlint. Preserve the
CI source pin and the prerelease channel's workflow/image identity checks.

Browser E2E requires a disposable loopback CPA instance, its own database and
management key, and an installed Chromium executable. Those tests modify route
configuration, prices, preferences, and usage history. Never use a shared or
production CPA instance.

The supervisor owns publication and human merge remains required. Preserve
managed task comments, enrolled labels, session keys, artifacts, and budgets
during recovery; report incompatible state rather than resetting it.
