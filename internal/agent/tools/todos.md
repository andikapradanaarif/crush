Manage a structured task list for multi-step work; each task has pending/in_progress/completed state. Keep exactly one task in_progress at a time. Skip for simple or single-step tasks.

Each item may carry:

- `key`: a stable slug identifying the item across rewrites. Required on items other items depend on — keep the key, reword freely.
- `depends_on`: keys of items that must complete first. Reference keys from this same list.
- `evidence_checks`: named checks that must resolve green for the item to count as done. Only configured names are bindable.
- `evidence_paths`: files or directories the work must touch — done requires an observed write and no covering check failed. Observed writes are file-tool writes (write/edit/multiedit/lsp_rename/lsp_replace_symbol), download targets, and bash redirect targets — mutations made by other means (sed -i, tee, codegen) are not visible here.

Bind evidence to items whose done-ness should be checkable rather than self-reported.
