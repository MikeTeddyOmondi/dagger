defmodule Dagger.CodegenTest do
  use ExUnit.Case
  doctest Dagger.Codegen

  alias Dagger.Codegen.Introspection.Types.Schema

  test "reads the schema version" do
    schema =
      Schema.from_map(%{
        "__schemaVersion" => "v1.0.0-beta.9",
        "__schema" => %{
          "queryType" => %{"name" => "Query"},
          "types" => []
        }
      })

    assert schema.version == "v1.0.0-beta.9"
  end
end
