package txs

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"strings"

	"github.com/bgentry/speakeasy"
	isatty "github.com/mattn/go-isatty"
	"github.com/pkg/errors"
	"github.com/spf13/viper"

	wire "github.com/tendermint/go-wire"
	"github.com/tendermint/go-wire/data"

	ctypes "github.com/tendermint/tendermint/rpc/core/types"

	sdk "github.com/cosmos/cosmos-sdk"
	"github.com/cosmos/cosmos-sdk/client/commands"
	"github.com/CyberMiles/travis/modules/auth"
	"github.com/ethereum/go-ethereum/common"
	ttypes "github.com/CyberMiles/travis/types"
)

// Validatable represents anything that can be Validated
type Validatable interface {
	ValidateBasic() error
}

func GetSigner() common.Address {
	address := viper.GetString(FlagAddress)

	if address == "" {
		fmt.Errorf("--address is required to sign tx")
		return common.Address{}
	}

	return common.HexToAddress(address)
}

// DoTx is a helper function for the lazy :)
//
// It uses only public functions and goes through the standard sequence of
// wrapping the tx with middleware layers, signing it, either preparing it,
// or posting it and displaying the result.
//
// If you want a non-standard flow, just call the various functions directly.
// eg. if you already set the middleware layers in your code, or want to
// output in another format.
func DoTx(tx sdk.Tx) (err error) {
	tx, err = Middleware.Wrap(tx)
	if err != nil {
		return err
	}

	err = SignTx(tx)
	if err != nil {
		return err
	}

	commit := viper.GetString(FlagType)
	if commit == "commit" {
		bres, err := PrepareOrPostTx(tx)
		if err != nil {
			return err
		}
		if bres == nil {
			return nil // successful prep, nothing left to do
		}
		return OutputTx(bres) // print response of the post

	} else {
		bres, err := PrepareOrPostTxSync(tx)
		if err != nil {
			return err
		}
		if bres == nil {
			return nil // successful prep, nothing left to do
		}
		return OutputTxSync(bres) // print response of the post
	}
}

func SignTx(tx sdk.Tx) error {
	// validate tx client-side
	err := tx.ValidateBasic()
	if err != nil {
		return err
	}

	// abort early if we don't want to sign
	if viper.GetBool(FlagNoSign) {
		return nil
	}

	address := viper.GetString(FlagAddress)

	if sign, ok := tx.Unwrap().(ttypes.Signable); ok {
		if address == "" {
			return errors.New("--address is required to sign tx")
		}
		err = signTx(sign, address)
	}
	return err
}

// PrepareOrPostTx checks the flags to decide to prepare the tx for future
// multisig, or to post it to the node. Returns error on any failure.
// If no error and the result is nil, it means it already wrote to file,
// no post, no need to do more.
func PrepareOrPostTx(tx sdk.Tx) (*ctypes.ResultBroadcastTxCommit, error) {
	wrote, err := PrepareTx(tx)
	// error in prep
	if err != nil {
		return nil, err
	}
	// successfully wrote the tx!
	if wrote {
		return nil, nil
	}
	// or try to post it
	return PostTx(tx)
}

func PrepareOrPostTxSync(tx sdk.Tx) (*ctypes.ResultBroadcastTx, error) {
	wrote, err := PrepareTx(tx)
	// error in prep
	if err != nil {
		return nil, err
	}
	// successfully wrote the tx!
	if wrote {
		return nil, nil
	}
	// or try to post it
	return PostTxSync(tx)
}

// PostTx does all work once we construct a proper struct
// it validates the data, signs if needed, transforms to bytes,
// and posts to the node.
func PostTxSync(tx sdk.Tx) (*ctypes.ResultBroadcastTx, error) {
	packet := wire.BinaryBytes(tx)
	// post the bytes
	node := commands.GetNode()
	return node.BroadcastTxSync(packet)
}


// PrepareTx checks for FlagPrepare and if set, write the tx as json
// to the specified location for later multi-sig.  Returns true if it
// handled the tx (no futher work required), false if it did nothing
// (and we should post the tx)
func PrepareTx(tx sdk.Tx) (bool, error) {
	prep := viper.GetString(FlagPrepare)
	if prep == "" {
		return false, nil
	}

	js, err := data.ToJSON(tx)
	if err != nil {
		return false, err
	}
	err = writeOutput(prep, js)
	if err != nil {
		return false, err
	}
	return true, nil
}

// PostTx does all work once we construct a proper struct
// it validates the data, signs if needed, transforms to bytes,
// and posts to the node.
func PostTx(tx sdk.Tx) (*ctypes.ResultBroadcastTxCommit, error) {
	packet := wire.BinaryBytes(tx)
	// post the bytes
	node := commands.GetNode()
	return node.BroadcastTxCommit(packet)
}

// OutputTx validates if success and prints the tx result to stdout
func OutputTx(res *ctypes.ResultBroadcastTxCommit) error {
	if res.CheckTx.IsErr() {
		return errors.Errorf("CheckTx: (%d): %s", res.CheckTx.Code, res.CheckTx.Log)
	}
	if res.DeliverTx.IsErr() {
		return errors.Errorf("DeliverTx: (%d): %s", res.DeliverTx.Code, res.DeliverTx.Log)
	}
	js, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(js))
	return nil
}

func OutputTxSync(res *ctypes.ResultBroadcastTx) error {
	js, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(js))
	return nil
}

func signTx(tx ttypes.Signable, address string) error {
	prompt := fmt.Sprintf("Please enter passphrase for %s: ", address)
	pass, err := getPassword(prompt)
	if err != nil {
		return err
	}
	return auth.Sign(tx, address, pass)
}

// if we read from non-tty, we just need to init the buffer reader once,
// in case we try to read multiple passwords
var buf *bufio.Reader

func inputIsTty() bool {
	return isatty.IsTerminal(os.Stdin.Fd()) || isatty.IsCygwinTerminal(os.Stdin.Fd())
}

func stdinPassword() (string, error) {
	if buf == nil {
		buf = bufio.NewReader(os.Stdin)
	}
	pass, err := buf.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(pass), nil
}

func getPassword(prompt string) (pass string, err error) {
	if inputIsTty() {
		pass, err = speakeasy.Ask(prompt)
	} else {
		pass, err = stdinPassword()
	}
	return
}

func writeOutput(file string, d []byte) error {
	var writer io.Writer
	if file == "-" {
		writer = os.Stdout
	} else {
		f, err := os.Create(file)
		if err != nil {
			return errors.WithStack(err)
		}
		defer f.Close()
		writer = f
	}

	_, err := writer.Write(d)
	// this returns nil if err == nil
	return errors.WithStack(err)
}

func readInput(file string) ([]byte, error) {
	var reader io.Reader
	// get the input stream
	if file == "" || file == "-" {
		reader = os.Stdin
	} else {
		f, err := os.Open(file)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		defer f.Close()
		reader = f
	}

	// and read it all!
	data, err := ioutil.ReadAll(reader)
	return data, errors.WithStack(err)
}
