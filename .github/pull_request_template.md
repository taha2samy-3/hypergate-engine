## Description
<!-- Describe the changes you made and the reason behind them. Include any issue numbers if applicable. -->
Fixes #

## AI Influence Level
<!-- Disclose if you used generative AI (like ChatGPT/Claude) to write or debug this code. -->
- [ ] **AIL:0** (No AI used)
- [ ] **AIL:1** (AI used for brainstorming/documentation)
- [ ] **AIL:2** (AI used for writing boilerplate/generating snippets)
- [ ] **AIL:3** (AI used for major parts of logic/debugging)

*If AIL is 1, 2, or 3, please briefly describe what you verified:*

## Type of Change
- [ ] **Feature** (New capability in Go Engine or Operator)
- [ ] **Bug Fix** (Resolves an issue in Go Engine or Operator)
- [ ] **Refactor** (Non-breaking code optimization)
- [ ] **Test Update** (Modified Python integration/E2E tests)
- [ ] **CRD/Manifests** (Updated Kubernetes Operator API definitions)
- [ ] **Documentation** (Markdown files/Routing guides)

## Quality Gate Checklist
- [ ] All commits contain a well-written description and follow **Conventional Commits** (e.g., `feat:`, `fix:`, `chore:`).
- [ ] PR title is descriptive and follows Conventional Commits format.
- [ ] Go formatting has been verified locally (`task hyper-engine-makefile:fmt` or `go fmt ./...`).
- [ ] Go static analysis checks passed locally (`task hyper-engine-makefile:vet`).
- [ ] If Operator APIs/CRDs were modified, the changes have been applied to files in `hyper-operator/deploy/CRDs/`.
- [ ] Python integration tests have been run locally using the corresponding Task (`task tests:test-api-key`, `task tests:test-ratelimit`, etc.).
- [ ] No hardcoded secrets, passwords, or tokens are present in the changes.
- [ ] I have read the project's CODEOWNERS and added the appropriate reviewers.
