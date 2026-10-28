/*
 *  MIT License
 *
 * Copyright (c) 2026 Jonas Kaninda
 *
 *  Permission is hereby granted, free of charge, to any person obtaining a copy
 *  of this software and associated documentation files (the "Software"), to deal
 *  in the Software without restriction, including without limitation the rights
 *  to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
 *  copies of the Software, and to permit persons to whom the Software is
 *  furnished to do so, subject to the following conditions:
 *
 *  The above copyright notice and this permission notice shall be included in all
 *  copies or substantial portions of the Software.
 *
 *  THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
 *  IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
 *  FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
 *  AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
 *  LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
 *  OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
 *  SOFTWARE.
 */

// Command libsql-bkup backs up and restores libSQL (sqld, libsql-server,
// Turso) databases as SQL dumps, streamed through bkupkit.
package main

import (
	"github.com/jkaninda/bkupkit/cli"
	"github.com/jkaninda/libsql-bkup/internal/libsql"
	"github.com/spf13/cobra"
)

// Version is injected at build time with -ldflags "-X main.Version=...".
var Version = "dev"

func main() {
	(&cli.App{
		Engine:  libsql.New(),
		Version: Version,
		Short:   "Back up and restore libSQL (sqld) databases to local, S3, GCS, Azure, SFTP or FTP storage",
		Extend:  examples,
	}).Main()
}

func examples(root *cobra.Command, _ *cli.App) {
	ex := map[string]string{
		"backup": "  libsql-bkup backup -d mydb\n" +
			"  libsql-bkup backup --storage s3 -d mydb --path backups/libsql\n" +
			"  libsql-bkup backup -d mydb --cron-expression '@daily'",
		"restore": "  libsql-bkup restore -d mydb -f mydb_20260101_120000.sql.gz\n" +
			"  libsql-bkup restore --storage s3 -d mydb --path backups/libsql -f mydb_20260101_120000.sql.gz.gpg --atomic",
	}
	for _, c := range root.Commands() {
		if e, ok := ex[c.Name()]; ok {
			c.Example = e
		}
	}
}
