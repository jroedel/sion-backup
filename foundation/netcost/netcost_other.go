//go:build !linux

package netcost

import "context"

// detect cannot tell on this platform.
//
// The answers exist and are not reachable from here. On Windows it is WinRT's
// NetworkInformation.GetInternetConnectionProfile().GetConnectionCost(), whose
// NetworkCostType distinguishes unrestricted, fixed and variable. On macOS it
// is NWPathMonitor's isExpensive, plus isConstrained for Low Data Mode. Both
// mean COM activation or Objective-C from a program that deliberately uses
// neither cgo nor a platform SDK.
//
// Returning Unknown rather than guessing is the whole point. A guess from the
// adapter name would call a USB-tethered phone and a USB dock the same thing,
// and would be wrong in the direction that costs somebody money.
//
// The way to say so on these platforms is the config file — see
// TuningConfig.Metered — which is a person telling the machine something the
// machine cannot find out, and is honest about being exactly that.
func detect(context.Context) Cost { return Unknown }

// supported is false here: see the comment on detect.
const supported = false
