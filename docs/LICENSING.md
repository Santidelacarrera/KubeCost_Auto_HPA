# Licensing review

Status: **findings only; no licence change made.** This is an engineering review, not legal advice. A legal review
is needed before commercial distribution.

## 1. Project licence

* `LICENSE` is the Apache License 2.0 full text. Git history shows one human author plus commits made with AI
  assistance (see §2).
* The appendix of `LICENSE` still contains the template line `Copyright [yyyy] [name of copyright owner]`. That
  appendix describes how to apply the licence to a file; it is not itself a notice. The project has **no copyright
  notice** yet (no `NOTICE` file and no per-file headers).
* **Open decision for the maintainer:** choose the copyright holder line (for example `Copyright 2026 <holder>`) and
  whether to add a `NOTICE` file. This review does not invent a holder.

## 2. Contributors and ownership

* Contributors in git history: the maintainer and Dependabot. Dependabot changes are dependency bumps.
* Contributions made through AI assistance are authored by the maintainer, who reviewed and merged them. Confirm this
  matches your organisation's policy before a commercial release.
* No contributor licence agreement (CLA) or DCO is enforced. Adding one is a maintainer decision.

## 3. Third-party dependencies (operator Go module)

Method (reproducible): `go mod download` for every module in `go list -m all` (run in `operator/`), then a text
match against the licence file found in each module directory. The result below was produced in this review.

| Licence (text match) | Modules |
|---|---|
| Apache-2.0 | 32 |
| MIT | 26 |
| BSD-3-Clause | 26 |
| ISC | 1 |
| Copyleft (GPL, LGPL, AGPL, MPL) | **0** |
| Unclassified or no licence file | **0** |

Total third-party modules: 85. The module list is in `go list -m all`; the classification script is not committed,
so rerun with the same method when dependencies change.

**Limitations.** The match is by licence text, not by an SPDX scanner, so it can misidentify a dual-licensed module.
Only the Go module graph of the operator is covered. The Python dependencies of `engine/` are **not** covered, and
the container base image packages are not reviewed here. An earlier version of this file reported most modules as
unclassified; that was a method error (modules had not been downloaded), now corrected.

## 4. Container base image

* The image is built on `gcr.io/distroless/static:nonroot`. Its contents are under the licences of the packages it
  contains (glibc/ca-certificates and similar). Include them in the SBOM review. Not checked here.

## 5. Practical consequences

* **Users (including enterprises):** Apache-2.0 allows commercial use, modification and internal deployment, with the
  licence and notices preserved. Patent terms apply as written in the licence.
* **Redistributors:** must ship `LICENSE` and any `NOTICE` content; must not imply endorsement. Once a holder line is
  chosen, include it in source and binaries.
* **Contributors:** their contributions are accepted under Apache-2.0 by the act of contributing, which is standard
  practice without a CLA.

## 6. Open items before a commercial release

1. Maintainer chooses the copyright holder line and whether to add `NOTICE`.
2. Run an SPDX-based scanner (for example `go-licenses`, which fails on the current Go 1.26 standard library here) on
   `operator/` and on the engine's Python dependencies, and review every non-permissive or unknown result.
3. Review the container base image's package licences from the SBOM.
4. Confirm the AI-assistance and contributor policy with the maintainer's organisation.
