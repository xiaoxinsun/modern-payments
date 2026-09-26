# Modern Payments — From Business Domain to Architecture

First-draft manuscript for an experienced software engineer, engineering manager, architect, or technical leader who is new to bank payments.

## Reading principle

The book follows a simple order:

1. **Business first** — why a payment capability exists and what business facts matter.
2. **Architecture second** — what system boundaries, state, and failure handling follow from those facts.
3. **Technology third** — implementation techniques are introduced only when the business problem justifies them.

The recurring fictional institution is **Global Trust Bank (GTB)**. Real payment infrastructures, standards, and institutions are called out as real examples and should be fact-checked before publication.

## Draft status

This is **Draft v0.1**. It was created from the project context currently available in ChatGPT and the approved book blueprint. The original long source conversation is in the same ChatGPT project, but not every turn is necessarily surfaced verbatim to the drafting context. As a result, this draft prioritizes the approved structure and the source themes that are available, while avoiding invented source-specific claims.

See [`notes/verification-ledger.md`](notes/verification-ledger.md) for claims that need primary-source verification before publication.

## License

Copyright © 2026 Bill Sun.

Except where otherwise noted, *Modern Payments — From Business Domain to Architecture* is licensed under the [Creative Commons Attribution 4.0 International License](https://creativecommons.org/licenses/by/4.0/).

You are free to share and adapt the material for any purpose, including commercially, provided that you give appropriate credit, link to the license, and indicate whether changes were made. See [`LICENSE`](LICENSE) for the complete license terms.

## Structure

- [`SUMMARY.md`](SUMMARY.md) — reading order / table of contents
- [`manuscript/`](manuscript/) — one Markdown file per chapter
- [`appendices/glossary.md`](appendices/glossary.md) — working glossary
- [`notes/verification-ledger.md`](notes/verification-ledger.md) — publication fact-check list

## GTB reference application

The GTB reference application is being implemented alongside the manuscript.
See [implementation progress](spec/gtb-implementation.md) and the
[backend developer guide](backend/README.md). With Go 1.27.1 installed, run
`make deps` then `make check` from this directory to validate the current backend.

## Suggested Git workflow

```bash
git init
git add .
git commit -m "Add first draft of Modern Payments manuscript"
git branch -M main
git remote add origin <your-repository-url>
git push -u origin main
```
