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

## 3. Third-party dependencies

Method: `go list -m all` in `operator/`, then the licence file found in each module directory.

| Result | Count | Notes |
|---|---|---|
| Apache-2.0 licence file found | 34 | Permissive |
| MIT licence file found | 20 | Permissive |
| BSD-style text found | 29 | Permissive (heuristic) |
| Other licence file found, not classified | remainder | Not fully classified |
| Modules with no recognisable licence file in the module directory | majority of the list | Cannot be classified by this method |
| Copyleft text (GPL, LGPL, AGPL, MPL) in any found licence file | 0 | Checked by text search |

**Limitation:** the search matched files named `LICENSE*` or `COPYING*` only, so many modules are unclassified. The
Go module zips do not always ship a licence file under those names. Do not conclude that these modules are
permissively licensed from this table. The correct tool is `go-licenses` (or an equivalent SBOM-based scanner) run
against the CycloneDX SBOM. That tool is **not installed here** and was not run.

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
2. Run `go-licenses report ./...` (or equivalent) on `operator/` and the Python engine's dependencies, and review
   every non-permissive or unknown result.
3. Review the container base image's package licences from the SBOM.
4. Confirm the AI-assistance and contributor policy with the maintainer's organisation.
