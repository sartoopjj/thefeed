package tgbridge

import "github.com/gotd/td/telegram/dcs"

// dcsTestList is Telegram's test data-centre list. The addresses in it are
// never dialled -- the tunnel resolver works from the DC id -- but gotd needs
// a list whose Test flag is set so it picks test-mode behaviour.
func dcsTestList() dcs.List { return dcs.Test() }
