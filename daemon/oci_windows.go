package daemon

import (
	"fmt"
	"io/ioutil"
	"path/filepath"
	"strings"
	"syscall"

	containertypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/container"
	"github.com/docker/docker/oci"
	"github.com/docker/docker/pkg/sysinfo"
	"github.com/opencontainers/runtime-spec/specs-go"

	"golang.org/x/sys/windows/registry"
)

func (daemon *Daemon) createSpec(c *container.Container) (*specs.Spec, error) {
	s := oci.DefaultSpec()

	linkedEnv, err := daemon.setupLinkedContainers(c)
	if err != nil {
		return nil, err
	}

	// Note, unlike Unix, we do NOT call into SetupWorkingDirectory as
	// this is done in VMCompute. Further, we couldn't do it for Hyper-V
	// containers anyway.

	// In base spec
	s.Hostname = c.FullHostname()

	if err := daemon.setupSecretDir(c); err != nil {
		return nil, err
	}

	if err := daemon.setupConfigDir(c); err != nil {
		return nil, err
	}

	// In s.Mounts
	mounts, err := daemon.setupMounts(c)
	if err != nil {
		return nil, err
	}

	var isHyperV bool
	if c.HostConfig.Isolation.IsDefault() {
		// Container using default isolation, so take the default from the daemon configuration
		isHyperV = daemon.defaultIsolation.IsHyperV()
	} else {
		// Container may be requesting an explicit isolation mode.
		isHyperV = c.HostConfig.Isolation.IsHyperV()
	}

	// If the container has not been started, and has configs or secrets
	// secrets, create symlinks to each confing and secret. If it has been
	// started before, the symlinks should have already been created. Also, it
	// is important to not mount a Hyper-V  container that has been started
	// before, to protect the host from the container; for example, from
	// malicious mutation of NTFS data structures.
	if !c.HasBeenStartedBefore && (len(c.SecretReferences) > 0 || len(c.ConfigReferences) > 0) {
		// The container file system is mounted before this function is called,
		// except for Hyper-V containers, so mount it here in that case.
		if isHyperV {
			if err := daemon.Mount(c); err != nil {
				return nil, err
			}
			defer daemon.Unmount(c)
		}
		if err := c.CreateSecretSymlinks(); err != nil {
			return nil, err
		}
		if err := c.CreateConfigSymlinks(); err != nil {
			return nil, err
		}
	}

	if m := c.SecretMounts(); m != nil {
		mounts = append(mounts, m...)
	}

	if m := c.ConfigMounts(); m != nil {
		mounts = append(mounts, m...)
	}

	for _, mount := range mounts {
		m := specs.Mount{
			Source:      mount.Source,
			Destination: mount.Destination,
		}
		if !mount.Writable {
			m.Options = append(m.Options, "ro")
		}
		s.Mounts = append(s.Mounts, m)
	}

	// In s.Process
	s.Process = &specs.Process{}
	s.Process.Args = append([]string{c.Path}, c.Args...)
	if !c.Config.ArgsEscaped {
		s.Process.Args = escapeArgs(s.Process.Args)
	}
	s.Process.Cwd = c.Config.WorkingDir
	if len(s.Process.Cwd) == 0 {
		// We default to C:\ to workaround the oddity of the case that the
		// default directory for cmd running as LocalSystem (or
		// ContainerAdministrator) is c:\windows\system32. Hence docker run
		// <image> cmd will by default end in c:\windows\system32, rather
		// than 'root' (/) on Linux. The oddity is that if you have a dockerfile
		// which has no WORKDIR and has a COPY file ., . will be interpreted
		// as c:\. Hence, setting it to default of c:\ makes for consistency.
		s.Process.Cwd = `C:\`
	}
	s.Process.Env = c.CreateDaemonEnvironment(c.Config.Tty, linkedEnv)
	s.Process.ConsoleSize = &specs.Box{
		Height: c.HostConfig.ConsoleSize[0],
		Width:  c.HostConfig.ConsoleSize[1],
	}
	s.Process.Terminal = c.Config.Tty
	s.Process.User.Username = c.Config.User

	// In spec.Root. This is not set for Hyper-V containers
	if !isHyperV {
		s.Root.Path = c.BaseFS
	}
	s.Root.Readonly = false // Windows does not support a read-only root filesystem

	// In s.Windows.Resources
	cpuShares := uint16(c.HostConfig.CPUShares)
	cpuMaximum := uint16(c.HostConfig.CPUPercent) * 100
	cpuCount := uint64(c.HostConfig.CPUCount)
	if c.HostConfig.NanoCPUs > 0 {
		if isHyperV {
			cpuCount = uint64(c.HostConfig.NanoCPUs / 1e9)
			leftoverNanoCPUs := c.HostConfig.NanoCPUs % 1e9
			if leftoverNanoCPUs != 0 {
				cpuCount++
				cpuMaximum = uint16(c.HostConfig.NanoCPUs / int64(cpuCount) / (1e9 / 10000))
				if cpuMaximum < 1 {
					// The requested NanoCPUs is so small that we rounded to 0, use 1 instead
					cpuMaximum = 1
				}
			}
		} else {
			cpuMaximum = uint16(c.HostConfig.NanoCPUs / int64(sysinfo.NumCPU()) / (1e9 / 10000))
			if cpuMaximum < 1 {
				// The requested NanoCPUs is so small that we rounded to 0, use 1 instead
				cpuMaximum = 1
			}
		}
	}
	memoryLimit := uint64(c.HostConfig.Memory)
	s.Windows.Resources = &specs.WindowsResources{
		CPU: &specs.WindowsCPUResources{
			Maximum: &cpuMaximum,
			Shares:  &cpuShares,
			Count:   &cpuCount,
		},
		Memory: &specs.WindowsMemoryResources{
			Limit: &memoryLimit,
		},
		Storage: &specs.WindowsStorageResources{
			Bps:  &c.HostConfig.IOMaximumBandwidth,
			Iops: &c.HostConfig.IOMaximumIOps,
		},
	}

	// Read and add credentials from the security options if a credential spec has been provided.
	if c.HostConfig.SecurityOpt != nil {
		cs := ""
		for _, sOpt := range c.HostConfig.SecurityOpt {
			sOpt = strings.ToLower(sOpt)
			if !strings.Contains(sOpt, "=") {
				return nil, fmt.Errorf("invalid security option: no equals sign in supplied value %s", sOpt)
			}
			var splitsOpt []string
			splitsOpt = strings.SplitN(sOpt, "=", 2)
			if len(splitsOpt) != 2 {
				return nil, fmt.Errorf("invalid security option: %s", sOpt)
			}
			if splitsOpt[0] != "credentialspec" {
				return nil, fmt.Errorf("security option not supported: %s", splitsOpt[0])
			}

			var (
				match   bool
				csValue string
				err     error
			)
			if match, csValue = getCredentialSpec("file://", splitsOpt[1]); match {
				if csValue == "" {
					return nil, fmt.Errorf("no value supplied for file:// credential spec security option")
				}
				if cs, err = readCredentialSpecFile(c.ID, daemon.root, filepath.Clean(csValue)); err != nil {
					return nil, err
				}
			} else if match, csValue = getCredentialSpec("registry://", splitsOpt[1]); match {
				if csValue == "" {
					return nil, fmt.Errorf("no value supplied for registry:// credential spec security option")
				}
				if cs, err = readCredentialSpecRegistry(c.ID, csValue); err != nil {
					return nil, err
				}
			} else {
				return nil, fmt.Errorf("invalid credential spec security option - value must be prefixed file:// or registry:// followed by a value")
			}
		}
		s.Windows.CredentialSpec = cs
	}

	return (*specs.Spec)(&s), nil
}

func escapeArgs(args []string) []string {
	escapedArgs := make([]string, len(args))
	for i, a := range args {
		escapedArgs[i] = syscall.EscapeArg(a)
	}
	return escapedArgs
}

// mergeUlimits merge the Ulimits from HostConfig with daemon defaults, and update HostConfig
// It will do nothing on non-Linux platform
func (daemon *Daemon) mergeUlimits(c *containertypes.HostConfig) {
	return
}

// getCredentialSpec is a helper function to get the value of a credential spec supplied
// on the CLI, stripping the prefix
func getCredentialSpec(prefix, value string) (bool, string) {
	if strings.HasPrefix(value, prefix) {
		return true, strings.TrimPrefix(value, prefix)
	}
	return false, ""
}

// readCredentialSpecRegistry is a helper function to read a credential spec from
// the registry. If not found, we return an empty string and warn in the log.
// This allows for staging on machines which do not have the necessary components.
func readCredentialSpecRegistry(id, name string) (string, error) {
	var (
		k   registry.Key
		err error
		val string
	)
	if k, err = registry.OpenKey(registry.LOCAL_MACHINE, credentialSpecRegistryLocation, registry.QUERY_VALUE); err != nil {
		return "", fmt.Errorf("failed handling spec %q for container %s - %s could not be opened", name, id, credentialSpecRegistryLocation)
	}
	if val, _, err = k.GetStringValue(name); err != nil {
		if err == registry.ErrNotExist {
			return "", fmt.Errorf("credential spec %q for container %s as it was not found", name, id)
		}
		return "", fmt.Errorf("error %v reading credential spec %q from registry for container %s", err, name, id)
	}
	return val, nil
}

// readCredentialSpecFile is a helper function to read a credential spec from
// a file. If not found, we return an empty string and warn in the log.
// This allows for staging on machines which do not have the necessary components.
func readCredentialSpecFile(id, root, location string) (string, error) {
	if filepath.IsAbs(location) {
		return "", fmt.Errorf("invalid credential spec - file:// path cannot be absolute")
	}
	base := filepath.Join(root, credentialSpecFileLocation)
	full := filepath.Join(base, location)
	if !strings.HasPrefix(full, base) {
		return "", fmt.Errorf("invalid credential spec - file:// path must be under %s", base)
	}
	bcontents, err := ioutil.ReadFile(full)
	if err != nil {
		return "", fmt.Errorf("credential spec '%s' for container %s as the file could not be read: %q", full, id, err)
	}
	return string(bcontents[:]), nil
}
