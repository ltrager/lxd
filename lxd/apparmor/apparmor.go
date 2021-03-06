package apparmor

import (
	"crypto/sha256"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"strings"

	"github.com/lxc/lxd/lxd/project"
	"github.com/lxc/lxd/lxd/state"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/logger"
	"github.com/lxc/lxd/shared/version"

	log "github.com/lxc/lxd/shared/log15"
)

const (
	cmdLoad   = "r"
	cmdUnload = "R"
	cmdParse  = "Q"
)

var aaPath = shared.VarPath("security", "apparmor")

type instance interface {
	Project() string
	Name() string
	IsNesting() bool
	IsPrivileged() bool
	ExpandedConfig() map[string]string
}

func mkApparmorName(name string) string {
	if len(name)+7 >= 253 {
		hash := sha256.New()
		io.WriteString(hash, name)
		return fmt.Sprintf("%x", hash.Sum(nil))
	}

	return name
}

// Namespace returns the instance's apparmor namespace.
func Namespace(c instance) string {
	/* / is not allowed in apparmor namespace names; let's also trim the
	 * leading / so it doesn't look like "-var-lib-lxd"
	 */
	lxddir := strings.Replace(strings.Trim(shared.VarPath(""), "/"), "/", "-", -1)
	lxddir = mkApparmorName(lxddir)
	name := project.Instance(c.Project(), c.Name())
	return fmt.Sprintf("lxd-%s_<%s>", name, lxddir)
}

// ProfileFull returns the instance's apparmor profile.
func ProfileFull(c instance) string {
	lxddir := shared.VarPath("")
	lxddir = mkApparmorName(lxddir)
	name := project.Instance(c.Project(), c.Name())
	return fmt.Sprintf("lxd-%s_<%s>", name, lxddir)
}

func profileShort(c instance) string {
	name := project.Instance(c.Project(), c.Name())
	return fmt.Sprintf("lxd-%s", name)
}

// profileContent generates the apparmor profile template from the given container.
// This includes the stock lxc includes as well as stuff from raw.apparmor.
func profileContent(state *state.State, c instance) (string, error) {
	// Prepare raw.apparmor.
	rawContent := ""
	rawApparmor, ok := c.ExpandedConfig()["raw.apparmor"]
	if ok {
		for _, line := range strings.Split(strings.Trim(rawApparmor, "\n"), "\n") {
			rawContent += fmt.Sprintf("  %s\n", line)
		}
	}

	// Render the profile.
	var sb *strings.Builder = &strings.Builder{}
	err := containerProfile.Execute(sb, map[string]interface{}{
		"feature_unix":     parserSupports("unix"),
		"feature_cgns":     shared.PathExists("/proc/self/ns/cgroup"),
		"feature_stacking": state.OS.AppArmorStacking && !state.OS.AppArmorStacked,
		"namespace":        Namespace(c),
		"nesting":          c.IsNesting(),
		"name":             ProfileFull(c),
		"unprivileged":     !c.IsPrivileged() || state.OS.RunningInUserNS,
		"raw":              rawContent,
	})
	if err != nil {
		return "", err
	}

	return sb.String(), nil
}

func runApparmor(state *state.State, command string, c instance) error {
	if !state.OS.AppArmorAvailable {
		return nil
	}

	output, err := shared.RunCommand("apparmor_parser", []string{
		fmt.Sprintf("-%sWL", command),
		path.Join(aaPath, "cache"),
		path.Join(aaPath, "profiles", profileShort(c)),
	}...)

	if err != nil {
		logger.Error("Running apparmor",
			log.Ctx{"action": command, "output": output, "err": err})
	}

	return err
}

func getCacheDir() string {
	basePath := path.Join(aaPath, "cache")

	ver, err := getVersion()
	if err != nil {
		logger.Errorf("Unable to get AppArmor version: %v", err)
		return basePath
	}

	// Multiple policy cache directories were only added in v2.13.
	minVer, err := version.NewDottedVersion("2.13")
	if err != nil {
		logger.Errorf("Unable to parse AppArmor version 2.13: %v", err)
		return basePath
	}

	if ver.Compare(minVer) < 0 {
		return basePath
	}

	output, err := shared.RunCommand("apparmor_parser", "-L", basePath, "--print-cache-dir")
	if err != nil {
		logger.Errorf("Unable to get AppArmor cache directory: %v", err)
		return basePath
	}

	return strings.TrimSpace(output)
}

func mkApparmorNamespace(state *state.State, c instance, namespace string) error {
	if !state.OS.AppArmorStacking || state.OS.AppArmorStacked {
		return nil
	}

	p := path.Join("/sys/kernel/security/apparmor/policy/namespaces", namespace)
	if err := os.Mkdir(p, 0755); !os.IsExist(err) {
		return err
	}

	return nil
}

// LoadProfile ensures that the instances's policy is loaded into the kernel so the it can boot.
func LoadProfile(state *state.State, c instance) error {
	if !state.OS.AppArmorAdmin {
		return nil
	}

	err := mkApparmorNamespace(state, c, Namespace(c))
	if err != nil {
		return err
	}

	/* In order to avoid forcing a profile parse (potentially slow) on
	 * every container start, let's use apparmor's binary policy cache,
	 * which checks mtime of the files to figure out if the policy needs to
	 * be regenerated.
	 *
	 * Since it uses mtimes, we shouldn't just always write out our local
	 * apparmor template; instead we should check to see whether the
	 * template is the same as ours. If it isn't we should write our
	 * version out so that the new changes are reflected and we definitely
	 * force a recompile.
	 */
	profile := path.Join(aaPath, "profiles", profileShort(c))
	content, err := ioutil.ReadFile(profile)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	updated, err := profileContent(state, c)
	if err != nil {
		return err
	}

	if string(content) != string(updated) {
		err = os.MkdirAll(path.Join(aaPath, "cache"), 0700)
		if err != nil {
			return err
		}

		err = os.MkdirAll(path.Join(aaPath, "profiles"), 0700)
		if err != nil {
			return err
		}

		err = ioutil.WriteFile(profile, []byte(updated), 0600)
		if err != nil {
			return err
		}
	}

	return runApparmor(state, cmdLoad, c)
}

// Destroy ensures that the instances's policy namespace is unloaded to free kernel memory.
// This does not delete the policy from disk or cache.
func Destroy(state *state.State, c instance) error {
	if !state.OS.AppArmorAdmin {
		return nil
	}

	if state.OS.AppArmorStacking && !state.OS.AppArmorStacked {
		p := path.Join("/sys/kernel/security/apparmor/policy/namespaces", Namespace(c))
		if err := os.Remove(p); err != nil {
			logger.Error("Error removing apparmor namespace", log.Ctx{"err": err, "ns": p})
		}
	}

	return runApparmor(state, cmdUnload, c)
}

// ParseProfile parses the profile without loading it into the kernel.
func ParseProfile(state *state.State, c instance) error {
	if !state.OS.AppArmorAvailable {
		return nil
	}

	return runApparmor(state, cmdParse, c)
}

// DeleteProfile removes the policy from cache/disk.
func DeleteProfile(state *state.State, c instance) {
	if !state.OS.AppArmorAdmin {
		return
	}

	/* It's ok if these deletes fail: if the container was never started,
	 * we'll have never written a profile or cached it.
	 */
	os.Remove(path.Join(getCacheDir(), profileShort(c)))
	os.Remove(path.Join(aaPath, "profiles", profileShort(c)))
}

func parserSupports(feature string) bool {
	ver, err := getVersion()
	if err != nil {
		logger.Errorf("Unable to get AppArmor version: %v", err)
		return false
	}

	if feature == "unix" {
		minVer, err := version.NewDottedVersion("2.10.95")
		if err != nil {
			logger.Errorf("Unable to parse AppArmor version 2.10.95: %v", err)
			return false
		}

		return ver.Compare(minVer) >= 0
	}

	return false
}

func getVersion() (*version.DottedVersion, error) {
	out, err := shared.RunCommand("apparmor_parser", "--version")
	if err != nil {
		return nil, err
	}

	fields := strings.Fields(strings.Split(out, "\n")[0])
	return version.NewDottedVersion(fields[len(fields)-1])
}
