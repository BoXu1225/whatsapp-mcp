def test_main_module_imports():
    import main

    assert main.mcp.name == "whatsapp"
