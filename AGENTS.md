# AGENTS.md

## Go Code Style

- Leave a blank line between top-level type and function declarations
- For multiline strings whose leading whitespace is significant, such as help text, use raw string literals
- If `editorconfig-checker` flags the leading whitespace in such a raw string literal, place `editorconfig-checker-disable` and `editorconfig-checker-enable` comments immediately before and after the block. Keep the exclusion limited to that block
- Format Go files with `gofmt`

## Commit Messages

- Write commit messages in English

## README and CLI Documentation

- Keep command examples, arguments, and environment variable descriptions in the README and usage text consistent with the implementation and tests. Evaluate examples such as Jsonnet and compare their output with any output shown in the documentation
- Describe option and environment variable precedence, including behavior for unset and empty values, accurately. Do not use phrases such as “as before” to imply a previous behavior that did not exist in the initial release
- Do not restore README explanations or cautions that the user has decided to remove unless explicitly requested
