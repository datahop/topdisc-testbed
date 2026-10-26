//go:build !linux

package host

type ethtool struct{}

func openPrimaryENA() (*ethtool, error) { return nil, errNotENA }

func (e *ethtool) ena() (ENASample, error) { return ENASample{}, errNotENA }

func (e *ethtool) Close() {}
