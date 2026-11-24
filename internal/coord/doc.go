// Package coord keeps every dcctl instance agreeing on the active
// datacenter: it reads dcctl/state from the voters, tells which record is in
// effect and aligns voters that lag behind.
package coord
