# Security policy

## Reporting a vulnerability

Report a suspected vulnerability privately, through GitHub's private vulnerability reporting: open the repository's **Security** tab and choose **Report a vulnerability**.

**Please do not open a public issue, a pull request or a discussion for a vulnerability**, and do not post details anywhere public before a fix is available. A public report gives everyone who reads the tracker the attack before anyone can run the fix.

Only you and the maintainers can see a private report.

## What to include

- What the problem is and which part of Hoserva it is in (the web UI or API, the CLI, a share, the container or template handling, the Unraid migration, backup and restore, an update or catalog download).
- The version or commit you tested, and whether the default configuration is affected or a non-default setting is needed.
- Who the attacker is and what they need to start with: someone on the local network with no account, a signed-in user, a user of a share, a local account on the server, or content you control, such as a template or an imported file.
- The steps to reproduce it, and what you were able to do with it.
- A suggested fix, if you have one. It is welcome and not required.

Reports that cannot be reproduced from the description are slower to act on, so please include exact commands or requests where you can.

## What happens next

1. You get an acknowledgement. This is a small, volunteer-run project, so please allow a few days.
2. The report is assessed against the project's published severity levels and either accepted or closed with an explanation.
3. An accepted report gets a fix. While no release has users, the fix is committed to the public `dev` branch with a neutral commit message that does not describe the problem, and the advisory stays private. Once a release has users, a Critical fix is developed in the advisory's temporary private fork and ships with the patched release. You are welcome to review the fix and to be credited in the advisory.
4. When the fix is on `main`, the advisory is published, with credit to you unless you ask not to be named. A CVE is requested where it is useful.

The report and its advisory are kept private until the fix is on `main`. If a report turns out not to be a vulnerability, or is a hardening suggestion with no way to exploit it today, it may be moved to a public issue with your agreement.

## Supported versions

Hoserva has not had a 1.0 release yet. Until it does, security fixes reach `main`, and only the latest release built from it is supported.

## Scope

In scope: the Hoserva daemon, CLI and web UI, the packages built from this repository, and the code that downloads and verifies updates and catalog archives.

Out of scope: vulnerabilities in the software Hoserva drives (mergerfs, SnapRAID, Samba, Docker and similar) — please report those to their own projects — and problems that need the administrator's own signed-in access to do something an administrator is allowed to do. Exposing the web UI to the internet is a choice Hoserva warns about; a weak password on an exposed instance is not a vulnerability in Hoserva.
