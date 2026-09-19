## Contributing to Hoserva

Hoserva is in its design phase — see `docs/internal/` for the architecture,
decisions and open questions, and the project's GitHub issues for current
work. `CLAUDE.md` at the repository root carries the full set of
architecture, safety and convention rules that apply to every change,
human or agent-made; read it before opening a pull request.

### Contribution terms: Developer Certificate of Origin, no CLA

Every commit must carry a `Signed-off-by:` trailer certifying the
[Developer Certificate of Origin](https://developercertificate.org/):

```
Signed-off-by: Jane Doe <jane@example.com>
```

`git commit -s` adds this automatically, or run `make hooks-install` once
after cloning and every commit gets it without having to remember `-s`. CI
rejects a pull request carrying a commit without one.

Hoserva does not use a Contributor License Agreement. There is no
relicensing intent behind this project, and a CLA would signal one — the
DCO is enough to establish provenance without it.

### Before opening a pull request

- Base your branch on `beta`, and open the pull request against `beta` —
  not `main`. `main` is release-only and only moves through the
  maintainer's own promotion (`docs/internal/12-repo-architecture.md §6`).
- Read `CLAUDE.md` and the design doc relevant to the area you're changing
  (the map is at the top of `CLAUDE.md`).
- Run `make build`, `make test` and `make lint` locally; CI runs the same
  checks.
- Follow the existing commit style (Conventional Commits, e.g.
  `fix(parity): ...`), one logical change per commit.
- If your change touches a settled decision (`Dn` in doc 00 §5) or a
  recommended default (`Qn` in doc 13), say so explicitly in the pull
  request description — reopening either needs a stated reason, not a
  silent divergence.

### License

Hoserva is licensed under the GNU Affero General Public License v3.0 (see
`LICENSE`). By contributing, you agree your contribution is licensed under
the same terms.
