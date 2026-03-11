const std = @import("std");

pub fn main() !void {
    // set up allocator
    var gpa: std.heap.GeneralPurposeAllocator(.{}) = .{};
    defer _ = gpa.deinit();
    const allocator = gpa.allocator();

    // set up stdout
    const stdout_file = std.fs.File.stdout();
    var stdout_buf: [4096]u8 = undefined;
    var fw = stdout_file.writer(&stdout_buf);
    const stdout = &fw.interface;

    // Get API key from environment variable
    const api_key = std.posix.getenv("OPENROUTER_API_KEY") orelse {
        std.debug.print("Error: OPENROUTER_API_KEY environment variable not set\n", .{});
        return;
    };

    // Build Authorization header value
    const auth_header = try std.fmt.allocPrint(allocator, "Bearer {s}", .{api_key});
    defer allocator.free(auth_header);

    // JSON request body
    const body =
        \\{
        \\  "model": "nvidia/nemotron-3-super-120b-a12b:free",
        \\  "messages": [
        \\    {
        \\      "role": "user",
        \\      "content": "How many r's are in the word 'strawberry'?"
        \\    }
        \\  ],
        \\  "reasoning": {
        \\    "enabled": false
        \\  }
        \\}
    ;

    // Create HTTP client
    var client: std.http.Client = .{ .allocator = allocator };
    defer client.deinit();

    // Use an Allocating writer to capture response body
    var aw: std.Io.Writer.Allocating = .init(allocator);
    defer aw.deinit();

    const result = try client.fetch(.{
        .location = .{ .url = "https://openrouter.ai/api/v1/chat/completions" },
        .method = .POST,
        .extra_headers = &.{
            .{ .name = "Content-Type", .value = "application/json" },
            .{ .name = "Authorization", .value = auth_header },
        },
        .payload = body,
        .response_writer = &aw.writer,
    });

    // Flush remaining buffered data
    try aw.writer.flush();

    // Get the collected response body
    var al = aw.toArrayList();
    defer al.deinit(allocator);
    const response_body = al.items;

    if (result.status != .ok) {
        stdout.print("HTTP Error: {}\n", .{result.status}) catch {};
        stdout.flush() catch {};
    }

    // Parse JSON response
    const parsed = std.json.parseFromSlice(std.json.Value, allocator, response_body, .{}) catch {
        // If JSON parsing fails, just print raw response
        stdout.print("{s}\n", .{response_body}) catch {};
        stdout.flush() catch {};
        return;
    };
    defer parsed.deinit();

    // Try to extract just the assistant's message content
    if (parsed.value.object.get("choices")) |choices_val| {
        if (choices_val.array.items.len > 0) {
            const first_choice = choices_val.array.items[0];
            if (first_choice.object.get("message")) |message| {
                if (message.object.get("content")) |content| {
                    switch (content) {
                        .string => |s| {
                            stdout.print("{s}\n", .{s}) catch {};
                            stdout.flush() catch {};
                            return;
                        },
                        else => {},
                    }
                }
            }
        }
    }

    // Fallback: print full raw response
    stdout.print("{s}\n", .{response_body}) catch {};
    stdout.flush() catch {};
}
