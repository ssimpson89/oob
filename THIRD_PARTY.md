# Third-party notices

## bauddy

Parts of the console bottom bar are adapted from bauddy
(https://github.com/ssimpson89/bauddy): the plain-text cleaner (`internal/console/termtext.go`),
the command-mode key map and hint-dropping status line logic (`internal/console/keys.go`,
`bar.go`), the log file naming and writer (`internal/console/logfile.go`), and the 10000-line
history idea (`internal/console/history.go`). Those files carry a header pointing here.

The MCP server (`oob mcp`) is adapted from bauddy's `internal/mcpserver`: the approval flow through MCP
elicitation (`internal/mcpserver/approve.go`), the list of commands that need approval and the prompt and
typed-text tracking behind it (`risk.go`, from `internal/paste` and `guard.go`), and the console session
model, read position and output shaping (`session.go`). Those files carry a header pointing here.
bauddy is by the same author and under the same license as oob (see LICENSE), so this
notice is for provenance rather than a separate obligation.

bauddy is licensed under the MIT License:

MIT License

Copyright (c) 2026 ssimpson89

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
