# gmail-filter-sync

[日本語](README.ja.md)

A tool for managing Gmail filters in Jsonnet files and syncing them through the `gws` command. The `gws` command is required at runtime.

## Installation

Download a binary for your OS and architecture from [GitHub Releases](https://github.com/ngyuki/gmail-filter-sync/releases).
Make it executable and place it in a directory on your PATH with the name `gmail-filter-sync`.

You can also install it with Go:

```sh
go install github.com/ngyuki/gmail-filter-sync@latest
```

## Setup

Download and install [`gws`](https://github.com/googleworkspace/cli/releases).
For a first-time setup, configure OAuth credentials with `gws auth setup` or follow the [manual setup instructions](https://github.com/googleworkspace/cli#authentication).
Then authenticate the target account with `gws auth login`.
Enable these two scopes in the authentication TUI:

- `gmail.settings.basic`: read, create, and delete filters
- `gmail.labels`: list labels

## Usage

Import the current settings, edit the file, review the diff, and apply the changes to Gmail.

```sh
gmail-filter-sync import # Import Gmail settings into a file
gmail-filter-sync edit   # Edit the settings file
gmail-filter-sync diff   # Review changes
gmail-filter-sync apply  # Apply changes to Gmail
```

`import` does not overwrite an existing file. Specify `--force` to update it. Imported settings are saved as JSON-compatible Jsonnet.

The filter settings file is selected in this order: `--file FILE`, the `GMAIL_FILTER_SYNC_FILE` environment variable, then the default path.
If `--file` is omitted and `GMAIL_FILTER_SYNC_FILE` is unset or empty, `$XDG_CONFIG_HOME/gmail-filter-sync/filter.jsonnet` is used.
If `XDG_CONFIG_HOME` is unset, empty, or a relative path, `~/.config/gmail-filter-sync/filter.jsonnet` is used instead.

```sh
export GMAIL_FILTER_SYNC_FILE=./filter.jsonnet
gmail-filter-sync diff
```

`edit` opens the file with `$EDITOR`. You can include arguments, for example, `EDITOR="code --wait"`. If `EDITOR` is unset, the command returns an error.
If the file does not exist, its parent directory is created and the editor is responsible for creating the file.

`diff` displays changes from the Gmail filter settings in unified diff format. `apply` applies those changes.

Set the `GMAIL_FILTER_SYNC_DIFF_FILTER` environment variable to a shell command to process diff output. The unified diff is passed to the command on standard input.
For example, set `GMAIL_FILTER_SYNC_DIFF_FILTER='colordiff | diff-highlight'` to display colored output. This applies to both `diff` and `apply`.

```sh
export GMAIL_FILTER_SYNC_DIFF_FILTER='colordiff | diff-highlight'
gmail-filter-sync diff
```

The target account is selected in this order: `--user USER`, then the `GMAIL_FILTER_SYNC_USER` environment variable.
If `--user` is omitted and `GMAIL_FILTER_SYNC_USER` is unset or empty, the authenticated `gws` user (`me`) is used.

```sh
export GMAIL_FILTER_SYNC_USER=user@example.com
gmail-filter-sync diff
```

## File format

See [example/filter.jsonnet](example/filter.jsonnet) for an example.
Each element of the `filters` array describes the [Gmail API criteria and action](https://developers.google.com/workspace/gmail/api/reference/rest/v1/users.settings.filters).

```jsonnet
local sender = 'newsletter@example.com';
{
  filters: [{
    criteria: { from: sender },
    action: {
      removeLabels: ['INBOX'],
      addLabels: ['STARRED'],
    },
  }],
}
```

`criteria` supports `from`, `to`, `subject`, `query`, `negatedQuery`, `hasAttachment`, `excludeChats`, `size`, and `sizeComparison`.
For size criteria, specify both `size` (in bytes) and `sizeComparison` (`larger` or `smaller`).

The `query` string is passed directly to Gmail. To combine multiple search terms, use a Jsonnet array and `std.join` to build the query string:

```jsonnet
local recipients = ['announce@example.com', 'no-reply@example.com'];
{
  filters: [{
    criteria: {
      query: 'from:{%s}' % std.join(' ', recipients),
    },
    action: { addLabels: ['STARRED'] },
  }],
}
```

This is passed to Gmail as `from:{announce@example.com no-reply@example.com}`.

`action` supports `addLabels`, `removeLabels`, and `forward`.
Specify labels by their names as displayed in Gmail. They are converted to IDs using the Gmail label list during sync. Names must match exactly, including capitalization. An error is returned if a label does not exist.

You can add at most one custom label per filter. Forwarding addresses must be verified in Gmail beforehand.
See the [official guide to managing filters](https://developers.google.com/workspace/gmail/api/guides/filter_settings) for details.

## Sync behavior

The file is treated as the complete set of filters for the account.
Existing filters not present in the file are deleted. Changes are applied by creating new filters and deleting old ones; filters with matching contents are left unchanged.
Filter and label ordering, empty strings, empty arrays, and the default value `false` do not produce a diff. Equivalent Gmail search queries are not detected.

To delete all filters, the file must contain `{ filters: [] }` and the `apply` subcommand must be given `--allow-empty`.

All additions are completed before deletions begin. If creation fails, existing filters are not deleted.
API operations are not transactional, so a partial failure may leave some changes in place.
Run `diff` again to inspect the state before applying further changes.

Because new filters are created before old ones are deleted, applying changes is rejected if this would exceed the limit of 1,000 filters. Old and new filters coexist temporarily.
After applying, the settings are fetched again to verify that they match the file.

## Tests

```sh
make check
make test
```

Tests mock `gws` responses and cover diff calculation, apply order, and stopping after failures. They do not apply changes to a real Gmail account.
