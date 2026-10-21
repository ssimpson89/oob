// Copyright (c) 2026 ssimpson89. MIT License. See THIRD_PARTY.md.
//
// The cleaner itself lives in internal/termtext (copied from bauddy's
// internal/termtext), shared with the paste pacer, which cannot import this
// package.

package console

import "github.com/ssimpson89/oob/internal/termtext"

// termCleaner turns a raw terminal byte stream into plain lines.
type termCleaner = termtext.Cleaner
