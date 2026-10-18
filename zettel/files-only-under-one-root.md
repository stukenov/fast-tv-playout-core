# A source may be a file only under SOURCE_FILE_ROOT, and a host only from the allow list

Assets are written by API clients, and the engine reads them with its own rights. `file:///etc/passwd` or `http://postgres:5432/` would turn a key with the `schedule`
scope into a read of the host and of the internal network. So `file://` works only under `SOURCE_FILE_ROOT` (symlinks and `..` that leave it are refused after resolving),
`http(s)` only to `SOURCE_ALLOW_HOSTS` when it is set, and the check is repeated on every redirect. Other schemes are refused. The default compose file mounts the media read-only.
