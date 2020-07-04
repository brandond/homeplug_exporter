// vim:ts=2:sw=2:et:ai:sts=2
package main

import (
	"errors"
	"fmt"
	"gopkg.in/alecthomas/kingpin.v2"
	"net"
)

type macAddressValue struct{ v *net.HardwareAddr }

func newMacAddressValue(p *net.HardwareAddr) *macAddressValue {
	return &macAddressValue{p}
}

func (f *macAddressValue) Set(s string) error {
	if s == "broadcast" || s == "all" {
		s = "ff:ff:ff:ff:ff:ff"
	}
	if s == "local" {
		s = "00:b0:52:00:00:01"
	}
	v, err := net.ParseMAC(s)
	if err == nil && len(v) != 6 {
		return errors.New("Invalid address length")
	}
	if err == nil {
		*f.v = (net.HardwareAddr)(v)
	}
	return err
}

func (f *macAddressValue) Get() interface{} { return (net.HardwareAddr)(*f.v) }

func (f *macAddressValue) String() string { return fmt.Sprintf("%v", *f.v) }

func MacAddress(s kingpin.Settings) (target *net.HardwareAddr) {
	target = &net.HardwareAddr{}
	s.SetValue(newMacAddressValue(target))
	return
}
