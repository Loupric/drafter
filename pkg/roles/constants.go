package roles

const (
	InitramfsName = "drafter.drftinitramfs"
	KernelName    = "drafter.drftkernel"
	DiskName      = "drafter.drftdisk"

	StateName  = "drafter.drftstate"
	MemoryName = "drafter.drftmemory"

	ConfigName = "drafter.drftconfig"
)

const (
	DefaultBootArgs = "console=ttyS0 panic=1 pci=off modules=ext4 rootfstype=ext4 i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd rootflags=rw printk.devkmsg=on printk_ratelimit=0 printk_ratelimit_burst=0"
)
