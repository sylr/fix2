{
  description = "fix2's Jepsen suite: the tools to run it from here";

  inputs = {
    nixpkgs.url = "https://flakehub.com/f/NixOS/nixpkgs/0.2605";
  };

  outputs = { self, nixpkgs }:
    let
      supportedSystems = [
        "x86_64-linux"
        "aarch64-linux"
        "x86_64-darwin"
        "aarch64-darwin"
      ];
      forEachSupportedSystem = f:
        nixpkgs.lib.genAttrs supportedSystems (system:
          let
            pkgs = import nixpkgs { inherit system; };
          in
          f { pkgs = pkgs; system = system; }
        );

      archMap = {
        "x86_64-linux"   = "linux-amd64";
        "aarch64-linux"  = "linux-arm64";
        "x86_64-darwin"  = "darwin-amd64";
        "aarch64-darwin" = "darwin-arm64";
      };

      githubBinaries = {
        # The version of infra/go.mod's Pulumi SDK.
        pulumi = {
          version = "3.270.0";
          url = v: arch: "https://github.com/pulumi/pulumi/releases/download/v${v}/pulumi-v${v}-${arch}.tar.gz";
          tarball = true;
          archMap = {
            "x86_64-linux"   = "linux-x64";
            "aarch64-linux"  = "linux-arm64";
            "x86_64-darwin"  = "darwin-x64";
            "aarch64-darwin" = "darwin-arm64";
          };
          sha256 = {
            "x86_64-linux"   = "d05899c251cb0413a9a2304f23229218c46852a61256913f34f08ebc27266139";
            "aarch64-linux"  = "b41f199a161aba4843312d4cda3143024fa6fbd73229d4149f572a1788f84bda";
            "x86_64-darwin"  = "20e38aa2710b1e1f6351787c51012757e2392bde547ab7430717094130672b56";
            "aarch64-darwin" = "688a9f4c5bb685cbd40b83e45c282715cb537ff2a5be97ffb6367ed6f6c26b34";
          };
        };
      };
    in
    {
      devShells = forEachSupportedSystem ({ pkgs, system }:
        let
          mkGithubBinary = name: spec:
            let
              specArchMap = if spec ? archMap then spec.archMap else archMap;
              isTarball = spec.tarball or false;
              tarballDepth = spec.tarballDepth or 2;
            in pkgs.stdenv.mkDerivation {
            pname = name;
            version = spec.version;
            src = pkgs.fetchurl {
              url = spec.url spec.version specArchMap.${system};
              sha256 = spec.sha256.${system};
            };
            dontUnpack = true;
            # Pulumi's tarball extracts to pulumi/: the CLI and its language
            # hosts, pulumi-language-go among them (tarballDepth = 2).
            installPhase = if isTarball then ''
              mkdir -p $out/bin
              tar -xzf $src -C $TMPDIR
              find $TMPDIR -mindepth ${toString tarballDepth} -maxdepth ${toString tarballDepth} -type f -perm -u+x -exec cp {} $out/bin/ \;
            '' else ''
              mkdir -p $out/bin
              cp $src $out/bin/${name}
              chmod +x $out/bin/${name}
            '';
          };

          # The control node's JDK (docker/control, and Ubuntu's on the VMs);
          # Leiningen on it, for the checker's tests and lein run.
          jdk = pkgs.jdk25;
          stablePackages = [
            jdk
            (pkgs.leiningen.override { inherit jdk; })
            pkgs.gnumake
          ];
          # Go is not here: nixpkgs' lags; the Go of the system builds the
          # node and infra/'s stacks.
          otherPackages = nixpkgs.lib.mapAttrsToList mkGithubBinary (nixpkgs.lib.filterAttrs (_: spec: spec.sha256 ? ${system}) githubBinaries);
        in
        {
          default = pkgs.mkShell {
            packages = stablePackages ++ otherPackages;
            JAVA_HOME = jdk.home;
            # The stacks' secrets, on the local backend: none by default
            # (see the Makefile).
            PULUMI_CONFIG_PASSPHRASE = "";
          };
        }
      );
    };
}
