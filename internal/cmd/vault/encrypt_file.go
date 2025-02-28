/*
Copyright © 2022 windvalley

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in
all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
THE SOFTWARE.
*/

package vault

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"

	"github.com/spf13/cobra"

	"github.com/windvalley/gossh/internal/pkg/aes"
	"github.com/windvalley/gossh/pkg/util"
)

var outputFile string

// encryptFileCmd represents the vault encrypt-file command
var encryptFileCmd = &cobra.Command{
	Use:   "encrypt-file",
	Short: "Encrypt a file",
	Long: `
Encrypt a file.`,
	Example: `
    # Encrypt a file by asking for vault password.
    $ gossh vault encrypt-file /path/auth.txt

    # Encrypt a file by vault password file.
    $ gossh vault encrypt-file /path/auth.txt -V /path/vault-password-file

    # Output encrypted content to another file.
    $ gossh vault encrypt-file /path/auth.txt -O /path/encryption.txt

    # Output encrypted content to screen.
    $ gossh vault encrypt-file /path/auth.txt -O -`,
	Args: func(cmd *cobra.Command, args []string) error {
		if len(args) < 1 {
			util.CobraCheckErrWithHelp(cmd, "requires one arg to represent a file to be encrypted")
		}

		if len(args) > 1 {
			util.CobraCheckErrWithHelp(cmd, "to many args, only need one")
		}

		if !util.FileExists(args[0]) {
			util.CheckErr(fmt.Sprintf("file '%s' not found", args[0]))
		}

		return nil
	},
	Run: func(cmd *cobra.Command, args []string) {
		vaultPass := getVaultConfirmPassword()

		file := args[0]

		p, err := ioutil.ReadFile(file)
		util.CheckErr(err)

		content := string(p)

		if aes.IsAES256CipherText(content) {
			util.CheckErr(fmt.Sprintf("file '%s' is already encrypted", file))
		}

		encryptContent, err := aes.AES256Encode(content, vaultPass)
		if err != nil {
			err = fmt.Errorf("encrypt failed: %w", err)
		}
		util.CheckErr(err)

		var (
			f    *os.File
			err1 error
		)

		if outputFile != "" {
			if outputFile == "-" {
				fmt.Println(encryptContent)
				fmt.Printf("\nEncryption successful\n")
				return
			}
			f, err1 = os.OpenFile(outputFile, os.O_CREATE|os.O_APPEND|os.O_RDWR, os.ModePerm)
		} else {
			f, err1 = os.OpenFile(file, os.O_TRUNC|os.O_RDWR, os.ModePerm)
		}
		util.CheckErr(err1)

		reader := bytes.NewReader([]byte(encryptContent))
		_, err = reader.WriteTo(f)
		util.CheckErr(err)

		fmt.Printf("\nEncryption successful\n")
	},
}

func init() {
	encryptFileCmd.Flags().StringVarP(
		&outputFile,
		"output-file",
		"O",
		"",
		"file that encrypted content is written to (use - for stdout)",
	)
}
