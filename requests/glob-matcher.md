# Feature request: a glob matcher package

Package `glob` (module path `github.com/acme/glob`), standard library only.

- `Compile(pattern string) (*Glob, error)` and `(*Glob) Match(path string) bool`.
- `*` matches any run of characters except `/`; `**` matches across `/` as well.
- `?` matches exactly one character except `/`.
- Character classes: `[abc]`, ranges `[a-z]`, negation `[!a-z]`.
- `\` escapes the next character, so `\*` matches a literal star.
- `Compile` must return an error for an unterminated class like `[abc`.
- Tests must cover every rule above, including `**` in the middle of a pattern
  such as `src/**/*_test.go`, and the escape rule.
