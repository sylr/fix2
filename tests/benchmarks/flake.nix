{
  description = "fix2 benchmarks: the toolchains of the agents";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";

  outputs =
    { nixpkgs, ... }:
    let
      systems = [
        "aarch64-darwin"
        "x86_64-darwin"
        "aarch64-linux"
        "x86_64-linux"
      ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});

      # QuickFIX C++, its latest release: nixpkgs has 1.15.1, of 2018. A
      # static library, so that its agent is one binary as the Go ones are,
      # optimised as CMake's Release builds are and nothing more: without
      # the hardening nixpkgs' compiler wrapper adds (stack protector,
      # registers zeroed on return, fortify, libc++'s checks), which an
      # upstream Release build has not.
      quickfixCpp =
        pkgs:
        pkgs.stdenv.mkDerivation {
          pname = "quickfix";
          version = "1.16.0";
          src = pkgs.fetchFromGitHub {
            owner = "quickfix";
            repo = "quickfix";
            rev = "v1.16.0";
            hash = "sha256-IVf7IxK/orqlI2RF8toJ6V0AuACpS78nHcDeR+CwK8c=";
          };
          nativeBuildInputs = [
            pkgs.cmake
            pkgs.ruby
            pkgs.libxslt
          ];
          # Its code generated from the benchmarks' dictionary, by its own
          # generator: the Ruby one writes the fields and the messages of
          # every version, xsltproc FIX 4.4's MessageCracker. Run from spec/,
          # without generate.sh, whose #!/bin/bash a Linux build sandbox has
          # not.
          postPatch = ''
            cp ${./spec/FIX44.xml} spec/FIX44.xml
            rm src/C++/fix44/*.h
            pushd spec
            ruby -I. Generator.rb > /dev/null
            xsltproc -o ../src/C++/fix44/MessageCracker.h MessageCracker.xsl FIX44.xml
            popd
          '';
          hardeningDisable = [ "all" ];
          cmakeFlags = [
            "-DCMAKE_BUILD_TYPE=Release"
            "-DQUICKFIX_SHARED_LIBS=OFF"
            "-DQUICKFIX_EXAMPLES=OFF"
            "-DQUICKFIX_TESTS=OFF"
          ];
        };
    in
    {
      packages = forAllSystems (pkgs: {
        quickfix-cpp = quickfixCpp pkgs;
      });

      devShells = forAllSystems (
        pkgs:
        let
          # Every agent's engine runs on the same JDK, the latest LTS.
          jdk = pkgs.jdk25;
          quickfix = quickfixCpp pkgs;
        in
        {
          default = pkgs.mkShell {
            packages = [
              pkgs.go_1_27
              jdk
              (pkgs.maven.override { jdk_headless = jdk; })
              (pkgs.gradle_9.override { java = jdk; })
              pkgs.cmake
              pkgs.gnumake
            ];
            # The C++ agent is built as QuickFIX C++ is, without hardening.
            hardeningDisable = [ "all" ];
            JAVA_HOME = jdk.home;
            # The Go of the shell, never one go.mod would download.
            GOTOOLCHAIN = "local";
            # Where agents/quickfixcpp finds QuickFIX C++.
            QUICKFIX_CPP = quickfix;
            QUICKFIX_CPP_VERSION = quickfix.version;
          };
        }
      );
    };
}
