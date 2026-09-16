# Contributing to apivo-news

## Entire.io Checkpoints

This project uses [Entire.io](https://entire.io) to capture AI-assisted development context.

### Setup (one-time per clone)

`ash
# For opencode
entire enable --agent opencode

# For claude-code
entire enable --agent claude-code
`

### How it works

1. Work with your agent (opencode/claude-code)
2. Stage and commit changes: git add -A && git commit -m "feat(...): ..."
3. Entire automatically links the session to the commit (no prompt — commit_linking: "always")
4. Push: git push — checkpoints sync to private repo

### Viewing checkpoints

- **CLI**: entire checkpoint list, entire checkpoint explain <id>
- **Web**: https://entire.io (connect GitHub, access private repo)

### CI validation

Every PR runs entire doctor to verify checkpoint health and sync status.