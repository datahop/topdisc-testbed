//go:build fork

package main

// testWire is two sessions' counters and their sum.
func testWire() (w1, w2, sum map[string]wireCounter) {
	return map[string]wireCounter{"FINDNODE/v5": {TxMsgs: 1, TxBytes: 10}},
		map[string]wireCounter{"FINDNODE/v5": {TxMsgs: 2, TxBytes: 5}, "PING/v5": {RxMsgs: 1}},
		map[string]wireCounter{"FINDNODE/v5": {TxMsgs: 3, TxBytes: 15}, "PING/v5": {RxMsgs: 1}}
}
