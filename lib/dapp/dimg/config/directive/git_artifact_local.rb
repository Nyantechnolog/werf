module Dapp
  module Dimg
    module Config
      module Directive
        # GitArtifactLocal
        class GitArtifactLocal < ArtifactBase
          alias add export
          undef_method :export
        end
      end
    end
  end
end
