## What changes

<!-- What is different for someone using dokkup after this is merged? -->

## Why

<!-- Link the issue, or explain the problem this solves. -->

## Checks

- [ ] `make lint`, `make test` and `make build` pass
- [ ] Commits are signed off (`git commit -s`) and follow Conventional Commits
- [ ] Documentation updated, if behaviour changed

<!-- Delete any section below that does not apply. -->

## Touches the Dokku seam

- [ ] New methods were added to `internal/dokku` **and** to the fake in the same
      change
- [ ] Commands are built as argument vectors, with no shell anywhere
- [ ] App names are validated before use

## Touches installation, removal or authentication

- [ ] What I did to convince myself this is safe:
- [ ] `make test-integration` output, or a description of what was verified by
      hand:

## Affects the weight budget

- [ ] Binary size, idle memory and start-up time are still within the budget in
      CONTRIBUTING.md — or the pull request explains why the budget should move
