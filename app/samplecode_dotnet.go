package main

// samplecode_dotnet.go — what every C# sample shares: the project file and how it is run. The
// programs themselves live beside the other languages' in each database's own file.
//
// Two decisions shape the project, and both come from the nodes rather than from .NET:
//
//   - It targets net8.0 and rolls forward. The seven base images do not agree on a .NET: Ubuntu
//     22.04's archive stops at 8, Oracle Linux and Ubuntu 24.04 ship 10, Debian ships none and
//     gets the 10 SDK archive. net8.0 is the one target every one of those SDKs can build, and
//     RollForward=Major lets the program run on the newest runtime present rather than failing
//     for want of an 8 runtime that a 10-only node never installed.
//   - It builds a framework-dependent dll with no apphost. `dotnet run` then starts it through
//     the `dotnet` muxer it found on PATH, which is the only arrangement that works unchanged
//     when the SDK is not in one of the directories an apphost probes (/usr/local/dotnet, on
//     Debian).
//
// The drivers are NuGet packages, restored from nuget.org into /root/.nuget/packages on the node
// at run time, under their own licences. DBCanvas pins their versions in the generated project and
// ships none of them.

// scDotnetRun is how every C# sample is executed. The restore is its own step, so the run does
// not repeat it.
const scDotnetRun = "dotnet run --no-restore"

// scDotnetCsproj is the project file for every C# sample: an executable, the package references
// the sample declares, and nothing it does not use.
const scDotnetCsproj = `<!--
{{xmlComment (.Header "  ")}}
-->
<Project Sdk="Microsoft.NET.Sdk">

  <PropertyGroup>
    <OutputType>Exe</OutputType>
    <!-- The one target every SDK on a DBCanvas node builds; RollForward runs it on the newest
         runtime installed, so a node with only .NET 10 does not need an 8 runtime too. -->
    <TargetFramework>net8.0</TargetFramework>
    <RollForward>Major</RollForward>
    <!-- A lab sample outlives its target's support window; the warning would say nothing new. -->
    <CheckEolTargetFramework>false</CheckEolTargetFramework>
    <!-- Start through the dotnet muxer rather than a native launcher, which has to find the
         runtime on its own and does not look where the Debian install puts it. -->
    <UseAppHost>false</UseAppHost>
    <ImplicitUsings>enable</ImplicitUsings>
    <Nullable>enable</Nullable>
  </PropertyGroup>

  <ItemGroup>
{{- range .Deps}}
    <PackageReference Include="{{.Name | xml}}" Version="{{.Version | xml}}" />
{{- end}}
  </ItemGroup>

</Project>
`

// scDotnetFiles is the two files of every C# project.
func scDotnetFiles(program string) func(g scGen) []scFile {
	return func(g scGen) []scFile {
		return []scFile{
			scFileOf("Program.cs", "csharp", program, g),
			scFileOf(scDotnetProject, "xml", scDotnetCsproj, g),
		}
	}
}
