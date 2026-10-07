package main

import "testing"

// The MySQL probe's batch comes back as \G output from several statements, some of them errors
// a flavour does not support; verticalFields has to read it as one set of answers.
func TestVerticalFields(t *testing.T) {
	out := `*************************** 1. row ***************************
ro: 1
*************************** 1. row ***************************
sro: 1
*************************** 1. row ***************************
dumps: 0
*************************** 1. row ***************************
             Replica_IO_State: Waiting for source to send event
                  Source_Host: mysql-1.example.net
           Replica_IO_Running: Yes
          Replica_SQL_Running: Yes
        Seconds_Behind_Source: 0
*************************** 1. row ***************************
               Slave_IO_State: Waiting for source to send event
                  Master_Host: mysql-1.example.net
`
	f := verticalFields(out)
	for k, want := range map[string]string{
		"ro": "1", "sro": "1", "dumps": "0", "Source_Host": "mysql-1.example.net",
		"Replica_IO_Running": "Yes", "Seconds_Behind_Source": "0", "Master_Host": "mysql-1.example.net",
		"Replica_IO_State": "Waiting for source to send event",
	} {
		if f[k] != want {
			t.Errorf("%s = %q, want %q", k, f[k], want)
		}
	}
	if _, ok := f["***************************"]; ok {
		t.Error("row separators must not become fields")
	}
}

func TestParseLiveIO(t *testing.T) {
	io := parseLiveIO("rios=4812\nwios=7474\nswap=0\nswapmax=max\nhostswap=0\n")
	if io.ReadOps != 4812 || io.WriteOps != 7474 {
		t.Fatalf("counters: %+v", io)
	}
	if io.SwapUsed == nil || *io.SwapUsed != 0 || io.SwapMax != nil || io.IOPressure != nil {
		t.Fatalf("swap/psi: %+v", io)
	}
	io = parseLiveIO("iopsi=12.50\nswapmax=1073741824\n")
	if io.IOPressure == nil || *io.IOPressure != 12.5 || io.SwapMax == nil || *io.SwapMax != 1<<30 {
		t.Fatalf("psi/limit: %+v", io)
	}
}
