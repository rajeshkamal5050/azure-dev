// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cmd

import (
	"fmt"
	"io"
	"strings"

	"microsoft.aspire/internal/version"

	"github.com/fatih/color"
)

// ASCII art using ANSI Shadow font for "ASPIRE".
// Visual width fits within 100 columns.
const bannerArt = `
 █████╗ ███████╗██████╗ ██╗██████╗ ███████╗
██╔══██╗██╔════╝██╔══██╗██║██╔══██╗██╔════╝
███████║███████╗██████╔╝██║██████╔╝█████╗  
██╔══██║╚════██║██╔═══╝ ██║██╔══██╗██╔══╝  
██║  ██║███████║██║     ██║██║  ██║███████╗
╚═╝  ╚═╝╚══════╝╚═╝     ╚═╝╚═╝  ╚═╝╚══════╝`

func printBanner(w io.Writer) {
	purple := color.RGB(81, 43, 212).Add(color.Bold) // Aspire brand purple (#512BD4)
	dim := color.New(color.Faint)
	fmt.Fprintln(w)

	for line := range strings.SplitSeq(bannerArt, "\n") {
		purple.Fprintln(w, line) //nolint:gosec // G104 - banner output errors are non-critical
	}

	dim.Fprintf(w, "v%s", version.Version) //nolint:gosec // G104 - banner output errors are non-critical
	fmt.Fprint(w, " ")
	fmt.Fprintln(w)
	dim.Fprintln(w, ".NET Aspire × Azure Developer CLI") //nolint:gosec // G104 - banner output errors are non-critical
	dim.Fprintln(w, "https://learn.microsoft.com/dotnet/aspire") //nolint:gosec // G104 - banner output errors are non-critical
	fmt.Fprintln(w)
}
