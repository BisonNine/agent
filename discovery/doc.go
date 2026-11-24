// Package discovery is progressive tool discovery over a toolkit registry:
// tools kept out of the model's tool list until a search finds them.
//
// Hidden and HideEntries mark tools hidden (AlwaysShown pins some listed);
// an Index searches an agent's hidden tools (BM25 or a regular expression)
// for the snapshot it is asked about; MetaTools provides search_tools and
// get_tool_details; ToolDefs gives the tool definitions for one model call,
// with a Session recording what tool results revealed. A hidden tool may
// always be called: discovery decides only what is listed. An MCP host
// serves every tool and never applies discovery.
//
// It imports only toolkit, toolspec and the standard library.
package discovery
