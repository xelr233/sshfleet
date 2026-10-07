package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// usageTemplateCN 是 cobra 默认用法模板(defaultUsageTemplate)的本地化版本:
// 只替换固定的英文提示,保留 rpad / FlagUsages 等原有行为。
const usageTemplateCN = `用法:{{if .Runnable}}
  {{.UseLine}}{{end}}{{if .HasAvailableSubCommands}}
  {{.CommandPath}} [command]{{end}}{{if gt (len .Aliases) 0}}

别名:
  {{.NameAndAliases}}{{end}}{{if .HasExample}}

示例:
{{.Example}}{{end}}{{if .HasAvailableSubCommands}}{{$cmds := .Commands}}{{if eq (len .Groups) 0}}

可用命令:{{range $cmds}}{{if (or .IsAvailableCommand (eq .Name "help"))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{else}}{{range $group := .Groups}}

{{.Title}}{{range $cmds}}{{if (and (eq .GroupID $group.ID) (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{if not .AllChildCommandsHaveGroup}}

其它命令:{{range $cmds}}{{if (and (eq .GroupID "") (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{end}}{{end}}{{if .HasAvailableLocalFlags}}

选项:
{{flagsCN .LocalFlags}}{{end}}{{if .HasAvailableInheritedFlags}}

全局选项:
{{flagsCN .InheritedFlags}}{{end}}{{if .HasHelpSubCommands}}

附加帮助主题:{{range .Commands}}{{if .IsAdditionalHelpTopicCommand}}
  {{rpad .CommandPath .CommandPathPadding}} {{.Short}}{{end}}{{end}}{{end}}{{if .HasAvailableSubCommands}}

使用 "{{.CommandPath}} [command] --help" 查看某命令的详细用法。{{end}}
`

// localize 把 cobra 自动生成的英文界面本地化为中文:
// 用法模板、版本模板、默认 help / completion 命令,以及 --help / --version 标志描述。
// 各业务命令自己的 Short/Long 不在此处理。
func localize(root *cobra.Command) {
	// pflag 的 FlagUsages 会在非零默认值后追加 "(default ...)",一并本地化。
	cobra.AddTemplateFunc("flagsCN", func(f *pflag.FlagSet) string {
		s := strings.ReplaceAll(f.FlagUsages(), "(default ", "(默认 ")
		return strings.TrimRight(s, "\n")
	})

	root.SetUsageTemplate(usageTemplateCN)
	root.SetVersionTemplate("sshf v{{.Version}}\n")

	// 预先声明 help / version 标志,cobra 检测到同名标志后便不再插入英文描述。
	// help 声明为持久标志,使所有子命令共用同一份中文描述。
	root.PersistentFlags().BoolP("help", "h", false, "显示帮助信息")
	root.Flags().BoolP("version", "v", false, "显示版本信息")

	root.SetHelpCommand(newHelpCmd())
	root.CompletionOptions.DisableDefaultCmd = true // 用本地化的 completion 命令替代
	root.AddCommand(newCompletionCmd(root))
}

// newHelpCmd 是 cobra 默认 help 命令的本地化替代。
func newHelpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "help [command]",
		Short: "查看任意命令的帮助",
		Long: `查看任意命令的帮助。

用法:sshf help <command>。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			target, _, err := cmd.Root().Find(args)
			if err != nil || target == nil {
				return fmt.Errorf("未知帮助主题: %s", strings.Join(args, " "))
			}
			target.InitDefaultHelpFlag()
			return target.Help()
		},
	}
}

// newCompletionCmd 是 cobra 默认 completion 命令的本地化替代,直接调用 cobra 的
// 补全脚本生成函数,因此脚本内容与官方行为一致,只是提示文案为中文。
func newCompletionCmd(root *cobra.Command) *cobra.Command {
	return &cobra.Command{
		Use:   "completion <bash|zsh|fish|powershell>",
		Short: "生成指定 shell 的自动补全脚本",
		Long: `生成 sshf 的 shell 自动补全脚本并打印到标准输出。

示例:
  source <(sshf completion bash)                                  # 当前 bash 会话
  sshf completion zsh  > "${fpath[1]}/_sshf"                      # zsh
  sshf completion fish > ~/.config/fish/completions/sshf.fish     # fish
  sshf completion powershell | Out-String | Invoke-Expression     # PowerShell`,
		Args:      exactArgs(1),
		ValidArgs: []string{"bash", "zsh", "fish", "powershell"},
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			switch args[0] {
			case "bash":
				return root.GenBashCompletionV2(out, true)
			case "zsh":
				return root.GenZshCompletion(out)
			case "fish":
				return root.GenFishCompletion(out, true)
			case "powershell":
				return root.GenPowerShellCompletionWithDesc(out)
			default:
				return fmt.Errorf("completion: 未知 shell %q(可选 bash/zsh/fish/powershell)", args[0])
			}
		},
	}
}

// 以下参数校验器替代 cobra.ExactArgs 等,把参数个数错误也变成中文。

func noArgs(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("命令 %q 不接受位置参数,但收到 %q", cmd.CommandPath(), args[0])
	}
	return nil
}

func exactArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) != n {
			return fmt.Errorf("命令 %q 需要 %d 个参数,实际收到 %d 个", cmd.CommandPath(), n, len(args))
		}
		return nil
	}
}

func minArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) < n {
			return fmt.Errorf("命令 %q 至少需要 %d 个参数,实际收到 %d 个", cmd.CommandPath(), n, len(args))
		}
		return nil
	}
}

func rangeArgs(min, max int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) < min || len(args) > max {
			return fmt.Errorf("命令 %q 需要 %d~%d 个参数,实际收到 %d 个", cmd.CommandPath(), min, max, len(args))
		}
		return nil
	}
}

// errTranslations 是 cobra / pflag 生成的英文错误片段 → 中文映射。
// 只做前缀/固定短语替换,其余内容(选项名、参数值等)原样保留。
var errTranslations = []struct{ en, cn string }{
	{"unknown shorthand flag: ", "未知短选项 "},
	{"unknown flag: ", "未知选项 "},
	{"flag needs an argument: ", "选项缺少参数 "},
	{"bad flag syntax: ", "选项语法错误 "},
	{"invalid argument ", "选项取值非法 "},
	{`" for "`, `" (选项 "`},
	{`" flag: `, `"): `},
	{"Did you mean this?", "你是不是想用:"},
	{"Did you mean one of these?", "你是不是想用其中之一:"},
}

// localizeError 把 cobra / pflag 产生的英文错误翻译成中文;无需翻译时原样返回。
// 注意:它不保留原错误的链,调用方应只在最终打印前使用。
func localizeError(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	// 未知命令消息形如 `unknown command "x" for "sshf"`(legacyArgs),单独处理,
	// 避免下面的 `" for "` 规则误伤。
	if strings.HasPrefix(msg, "unknown command ") {
		msg = strings.Replace(msg, "unknown command ", "未知命令 ", 1)
		msg = strings.Replace(msg, " for ", ",父命令 ", 1)
	}
	for _, t := range errTranslations {
		msg = strings.ReplaceAll(msg, t.en, t.cn)
	}
	if msg == err.Error() {
		return err
	}
	return fmt.Errorf("%s", msg)
}
