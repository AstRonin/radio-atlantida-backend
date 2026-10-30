package media

import "time"

// processWaitDelay bounds the wait for a cancelled process: after the group is
// killed, its pipes may still be held by a descendant that outlived it, and
// Wait closes them after this delay instead of blocking.
const processWaitDelay = 5 * time.Second
