package rvn

import (
	"encoding/json"
	"fmt"
	"github.com/libvirt/libvirt-go"
	xlibvirt "github.com/libvirt/libvirt-go-xml"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"strings"
)

/*~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~
 *
 * Public API Implementation
 *
 *~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~*/

// Create creates a libvirt definition for the supplied topology.  It does not
// launch the system. For that functionality use the Launch function. If a
// topology with the same name as the topology provided as an argument exists,
// that topology will be overwritten by the system generated from the argument.
func Create(topo Topo) {
	topoDir := SysDir() + "/" + topo.Name
	os.MkdirAll(topoDir, 0755)

	doms := make(map[string]*xlibvirt.Domain)
	nets := make(map[string]*xlibvirt.Network)

	subnet := LoadRuntime().AllocateSubnet(topo.Name)
	topo.MgmtIp = fmt.Sprintf("172.22.%d.1", subnet)

	nets["test"] = &xlibvirt.Network{
		Name: topo.QualifyName("test"),
		IPs: []xlibvirt.NetworkIP{
			xlibvirt.NetworkIP{
				Address: topo.MgmtIp,
				Netmask: "255.255.255.0",
				DHCP: &xlibvirt.NetworkDHCP{
					Ranges: []xlibvirt.NetworkDHCPRange{
						xlibvirt.NetworkDHCPRange{
							Start: fmt.Sprintf("172.22.%d.2", subnet),
							End:   fmt.Sprintf("172.22.%d.254", subnet),
						},
					},
				},
			},
		},
		Domain: &xlibvirt.NetworkDomain{
			Name:      topo.Name + ".net",
			LocalOnly: "yes",
		},
		Forward: &xlibvirt.NetworkForward{
			Mode: "nat",
		},
	}

	for _, node := range topo.Nodes {
		d := newDom(&node.Host, &topo)
		runHooks(d)
		genConfig(node.Host, topo)
		doms[node.Name] = d
		domConnect(topo.QualifyName("test"), &node.Host, d, nil)
	}

	for _, zwitch := range topo.Switches {
		d := newDom(&zwitch.Host, &topo)
		runHooks(d)
		genConfig(zwitch.Host, topo)
		doms[zwitch.Name] = d
		domConnect(topo.QualifyName("test"), &zwitch.Host, d, nil)
	}

	for _, link := range topo.Links {
		n := &xlibvirt.Network{
			Name:   topo.QualifyName(link.Name),
			Bridge: &xlibvirt.NetworkBridge{Delay: "0", STP: "off"},
		}

		for _, e := range link.Endpoints {
			d := doms[e.Name]
			domConnect(topo.QualifyName(link.Name), topo.getHost(e.Name), d, link.Props)
		}

		nets[link.Name] = n
	}

	data, _ := json.MarshalIndent(topo, "", "  ")
	ioutil.WriteFile(topoDir+"/"+topo.Name+".json", []byte(data), 0644)

	conn, err := libvirt.NewConnect("qemu:///system")
	if err != nil {
		log.Printf("libvirt connect failure: %v", err)
		return
	}
	defer conn.Close()

	for _, d := range doms {
		xml, err := d.Marshal()
		if err != nil {
			log.Printf("error marshalling domain %v", err)
		}
		ioutil.WriteFile(topoDir+"/dom_"+d.Name+".xml", []byte(xml), 0644)

		dd, err := conn.LookupDomainByName(d.Name)
		if err != nil {
			_, err := conn.DomainDefineXML(xml)
			if err != nil {
				log.Printf("error defining domain %v", err)
			}
		} else {
			dd.Destroy()
			dd.Undefine()
			conn.DomainDefineXML(xml)
			dd.Free()
		}
	}

	for _, n := range nets {
		xml, _ := n.Marshal()
		ioutil.WriteFile(topoDir+"/net_"+n.Name+".xml", []byte(xml), 0644)

		nn, err := conn.LookupNetworkByName(n.Name)
		if err != nil {
			conn.NetworkDefineXML(xml)
		} else {
			nn.Destroy()
			nn.Undefine()
			conn.NetworkDefineXML(xml)
			nn.Free()
		}
	}

	//create NFS exports
	ExportNFS(topo)

}

// Destroy completely wipes out a topology with the given name. If the system
// is running within libvirt it is torn down. The entire definition of the
// system is also removed from libvirt.
func Destroy(topoName string) {
	conn, err := libvirt.NewConnect("qemu:///system")
	if err != nil {
		log.Printf("libvirt connect failure: %v", err)
		return
	}
	defer conn.Close()

	topo, err := loadTopo(topoName)
	if err != nil {
		log.Printf("destroy: failed to load topo %s", topoName)
		return
	}
	topoDir := SysDir() + "/" + topo.Name
	exec.Command("rm", "-rf", topoDir).Run()

	for _, x := range topo.Nodes {
		destroyDomain(topo.QualifyName(x.Name), conn)
	}
	for _, x := range topo.Switches {
		destroyDomain(topo.QualifyName(x.Name), conn)
	}

	for _, x := range topo.Links {
		destroyNetwork(topo.QualifyName(x.Name), conn)
	}
	destroyNetwork(topo.QualifyName("test"), conn)
	LoadRuntime().FreeSubnet(topo.Name)
	UnexportNFS(topoName)
}

// Launch brings up the system with the given name. This system must exist
// visa-vis the create function before calling Launch. The return value is
// a list of diagnostic strings that were provided by libvirt when launching
// the system. The existence of diagnostics does not necessarily indicate
// an error in launching. This function is asynchronous, when it returns the
// system is still launching. Use the Status function to check up on a the
// launch process.
func Launch(topoName string) []string {
	conn, err := libvirt.NewConnect("qemu:///system")
	if err != nil {
		log.Printf("libvirt connect failure: %v", err)
		return []string{fmt.Sprintf("%v", err)}
	}
	defer conn.Close()

	topo, err := loadTopo(topoName)
	if err != nil {
		err := fmt.Errorf("failed to load topo %s - %v", topoName, err)
		return []string{fmt.Sprintf("%v", err)}
	}

	//collect all the doamins and networks first so we know everything we need
	//exists
	var errors []string
	var doms []*libvirt.Domain
	var nets []*libvirt.Network

	for _, x := range topo.Nodes {
		d, err := conn.LookupDomainByName(topo.QualifyName(x.Name))
		if err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", x.Name, err))
		} else {
			doms = append(doms, d)
		}
	}
	for _, x := range topo.Switches {
		d, err := conn.LookupDomainByName(topo.QualifyName(x.Name))
		if err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", x.Name, err))
		} else {
			doms = append(doms, d)
		}
	}

	for _, x := range topo.Links {
		n, err := conn.LookupNetworkByName(topo.QualifyName(x.Name))
		if err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", x.Name, err))
		} else {
			nets = append(nets, n)
		}
	}

	//test network
	n, err := conn.LookupNetworkByName(topo.QualifyName("test"))
	if err != nil {
		errors = append(errors, fmt.Sprintf("%s: %v", "test", err))
	} else {
		nets = append(nets, n)
	}

	for _, net := range nets {
		err := net.Create()
		name, _ := net.GetName()
		if err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", name, err))
		}
		if name != topo.QualifyName("test") {
			setBridgeProperties(net)
		}
		net.Free()
	}

	for _, dom := range doms {
		err := dom.Create()
		if err != nil {
			name, _ := dom.GetName()
			errors = append(errors, fmt.Sprintf("%s: %v", name, err))
		}
		dom.Free()
	}

	return errors
}

// DomStatus encapsulates various information about a libvirt domain for
// purposes of serialization and presentation.
type DomStatus struct {
	State string
	IP    string
	VNC   int
}

// The status function returns the runtime status of a topology, node by node
// and network by network.
func Status(topoName string) map[string]interface{} {

	status := make(map[string]interface{})

	conn, err := libvirt.NewConnect("qemu:///system")
	if err != nil {
		log.Printf("libvirt connect failure: %v", err)
		return status
	}
	defer conn.Close()

	topo, err := loadTopo(topoName)
	if err != nil {
		log.Printf("status: failed to load topo %s - %v", topoName, err)
		return status
	}

	nodes := make(map[string]DomStatus)
	status["nodes"] = nodes

	switches := make(map[string]DomStatus)
	status["switches"] = switches

	links := make(map[string]string)
	status["links"] = links

	for _, x := range topo.Nodes {
		nodes[x.Name] = domainStatus(topo.QualifyName(x.Name), conn)
	}
	for _, x := range topo.Switches {
		switches[x.Name] = domainStatus(topo.QualifyName(x.Name), conn)
	}

	for _, x := range topo.Links {
		links[x.Name] = networkStatus(topo.QualifyName(x.Name), conn)
	}
	return status
}

func DomainStatus(name string) (DomStatus, error) {
	conn, err := libvirt.NewConnect("qemu:///system")
	if err != nil {
		log.Printf("libvirt connect failure: %v", err)
		return DomStatus{}, err
	}
	defer conn.Close()

	return domainStatus(name, conn), nil
}

/*~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~
 *
 * Helper functions
 *
 *~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~*/

func mountDirs(h *Host, d *xlibvirt.Domain) {
	for i, mount := range h.Mounts {
		tag := d.Name + strings.Replace(mount.Point, "/", "-", -1)
		h.Mounts[i].Tag = tag
		d.Devices.Filesystems = append(d.Devices.Filesystems,
			xlibvirt.DomainFilesystem{
				Type:       "mount",
				AccessMode: "mapped",
				Driver: &xlibvirt.DomainFilesystemDriver{
					Type:     "path",
					WRPolicy: "immediate",
				},
				Source: &xlibvirt.DomainFilesystemSource{
					Dir: mount.Source,
				},
				Target: &xlibvirt.DomainFilesystemTarget{
					Dir: tag,
				},
			})
	}
}

func newDom(h *Host, t *Topo) *xlibvirt.Domain {

	baseImage := "/var/rvn/img/" + h.Image + ".qcow2"
	instanceImage := SysDir() + "/" + t.Name + "/" + h.Name + ".qcow2"
	exec.Command("rm", "-f", instanceImage).Run()

	err := exec.Command(
		"qemu-img",
		"create",
		"-f",
		"qcow2",
		"-o", "backing_file="+baseImage,
		instanceImage).Run()

	if err != nil {
		log.Printf("error creating image file for %s", h.Name)
		log.Printf("%s", err)
	}

	d := &xlibvirt.Domain{
		Type: "kvm",
		Name: t.QualifyName(h.Name),
		Features: &xlibvirt.DomainFeatureList{
			ACPI: &xlibvirt.DomainFeature{},
			APIC: &xlibvirt.DomainFeatureAPIC{},
		},
		OS: &xlibvirt.DomainOS{
			Type: &xlibvirt.DomainOSType{Type: "hvm"},
			//BootDevices: []xlibvirt.DomainBootDevice{
			//xlibvirt.DomainBootDevice{Dev: "hd"},
			//xlibvirt.DomainBootDevice{Dev: "network"},
			//},
		},
		Memory: &xlibvirt.DomainMemory{Value: 1024, Unit: "MiB"},
		Devices: &xlibvirt.DomainDeviceList{
			Serials: []xlibvirt.DomainChardev{
				xlibvirt.DomainChardev{
					Type: "pty",
				},
			},
			Consoles: []xlibvirt.DomainChardev{
				xlibvirt.DomainChardev{
					Type:   "pty",
					Target: &xlibvirt.DomainChardevTarget{Type: "serial"},
				},
			},
			Graphics: []xlibvirt.DomainGraphic{
				xlibvirt.DomainGraphic{
					Type:     "vnc",
					Port:     -1,
					AutoPort: "yes",
				},
			},
			Disks: []xlibvirt.DomainDisk{
				xlibvirt.DomainDisk{
					Type:   "file",
					Device: "disk",
					Driver: &xlibvirt.DomainDiskDriver{Name: "qemu", Type: "qcow2"},
					Source: &xlibvirt.DomainDiskSource{
						File: instanceImage,
					},
					Target: &xlibvirt.DomainDiskTarget{Dev: "vda", Bus: "virtio"},
				},
			},
		},
	}

	mountDirs(h, d)

	return d
}

func domConnect(net string, h *Host, dom *xlibvirt.Domain, props map[string]interface{}) {

	var boot *xlibvirt.DomainInterfaceBoot = nil
	if strings.ToLower(h.OS) == "netboot" {
		if props != nil {
			boot_order, ok := props["boot"]
			if ok {
				boot_order_num, ok := boot_order.(float64)
				if ok {
					boot = &xlibvirt.DomainInterfaceBoot{
						Order: int(boot_order_num),
					}
				}
			}
		}
	}
	dom.Devices.Interfaces = append(dom.Devices.Interfaces,
		xlibvirt.DomainInterface{
			Type:   "network",
			Source: &xlibvirt.DomainInterfaceSource{Network: net},
			Model:  &xlibvirt.DomainInterfaceModel{Type: "virtio"},
			Boot:   boot,
		})
}

func loadTopo(name string) (Topo, error) {
	topoDir := SysDir() + "/" + name
	path := topoDir + "/" + name + ".json"
	return LoadTopo(path)
}

func domainStatus(name string, conn *libvirt.Connect) DomStatus {
	var status DomStatus
	x, err := conn.LookupDomainByName(name)
	if err != nil {
		status.State = "non-existant"
	} else {
		info, _ := x.GetInfo()
		switch info.State {
		case libvirt.DOMAIN_NOSTATE:
			status.State = "nostate"
		case libvirt.DOMAIN_RUNNING:
			status.State = "running"
			addrs, err := x.ListAllInterfaceAddresses(
				libvirt.DOMAIN_INTERFACE_ADDRESSES_SRC_LEASE)
			if err == nil && len(addrs) > 0 {
				ifx := addrs[0]
				if len(ifx.Addrs) > 0 {
					status.IP = ifx.Addrs[0].Addr
				}
			}
		case libvirt.DOMAIN_BLOCKED:
			status.State = "blocked"
		case libvirt.DOMAIN_PAUSED:
			status.State = "paused"
		case libvirt.DOMAIN_SHUTDOWN:
			status.State = "shutdown"
		case libvirt.DOMAIN_CRASHED:
			status.State = "crashed"
		case libvirt.DOMAIN_PMSUSPENDED:
			status.State = "suspended"
		case libvirt.DOMAIN_SHUTOFF:
			status.State = "off"
		}
		x.Free()
	}
	return status
}

func networkStatus(name string, conn *libvirt.Connect) string {
	x, err := conn.LookupNetworkByName(name)
	if err != nil {
		return "non-existant"
	} else {
		active, _ := x.IsActive()
		if active {
			return "up"
		} else {
			return "down"
		}
		x.Free()
	}
	return "?"
}

func destroyDomain(name string, conn *libvirt.Connect) {
	x, err := conn.LookupDomainByName(name)
	if err != nil {
		//ok nothing to destroy
	} else {
		x.Destroy()
		x.Undefine()
		x.Free()
	}
}

func destroyNetwork(name string, conn *libvirt.Connect) {
	x, err := conn.LookupNetworkByName(name)
	if err != nil {
		//ok nothing to destroy
	} else {
		x.Destroy()
		x.Undefine()
		x.Free()
	}
}

func setBridgeProperties(net *libvirt.Network) {
	allowLLDP(net)
	allowBOOTP(net)
}

func allowLLDP(net *libvirt.Network) {
	name, _ := net.GetName()
	br, err := net.GetBridgeName()
	if err != nil {
		log.Printf("error getting bridge for %s - %v", name, err)
		return
	}

	err = ioutil.WriteFile(
		fmt.Sprintf("/sys/class/net/%s/bridge/group_fwd_mask", br),
		[]byte("16384"),
		0644,
	)

	if err != nil {
		log.Printf("unable to set group forwarding mask on bridge %s - %v",
			name,
			err,
		)
		return
	}
}

func allowBOOTP(net *libvirt.Network) {
	name, _ := net.GetName()
	br, err := net.GetBridgeName()
	if err != nil {
		log.Printf("error getting bridge for %s - %v", name, err)
		return
	}

	out, err := exec.Command("iptables", "-A", "FORWARD",
		"-i", br,
		"-d", "255.255.255.255",
		"-j", "ACCEPT").CombinedOutput()

	if err != nil {
		log.Printf("error allowing bootp through iptables %s - %v", out, err)
		return
	}

}
