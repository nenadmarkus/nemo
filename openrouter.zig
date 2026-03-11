const std = @import("std");

fn InvokeIntelligence(
    allocator: std.mem.Allocator,
    api_key: []const u8,
    prompt: []const u8,
) ![]u8 {
    const auth_header = try std.fmt.allocPrint(allocator, "Bearer {s}", .{api_key});
    defer allocator.free(auth_header);

    const body = try std.fmt.allocPrint(allocator,
        \\{{
        \\  "model": "nvidia/nemotron-3-super-120b-a12b:free",
        \\  "messages": [{{"role": "user", "content": {f}}}],
        \\  "reasoning": {{"enabled": false}}
        \\}}
    , .{std.json.fmt(prompt, .{})});
    defer allocator.free(body);

    var client: std.http.Client = .{ .allocator = allocator };
    defer client.deinit();

    var aw: std.Io.Writer.Allocating = .init(allocator);
    defer aw.deinit();

    const result = try client.fetch(.{
        .location = .{ .url = "https://openrouter.ai/api/v1/chat/completions" },
        .method = .POST,
        .headers = .{
            .content_type = .{ .override = "application/json" },
            .authorization = .{ .override = auth_header },
        },
        .payload = body,
        .response_writer = &aw.writer,
    });

    const response_bytes = aw.writer.buffer[0..aw.writer.end];

    if (result.status != .ok) {
        return std.fmt.allocPrint(allocator, "HTTP Error: {}\n{s}", .{ result.status, response_bytes });
    }

    const parsed = std.json.parseFromSlice(std.json.Value, allocator, response_bytes, .{}) catch {
        return try allocator.dupe(u8, response_bytes);
    };
    defer parsed.deinit();

    if (parsed.value.object.get("choices")) |choices_val| {
        if (choices_val.array.items.len > 0) {
            const first_choice = choices_val.array.items[0];
            if (first_choice.object.get("message")) |message| {
                if (message.object.get("content")) |content| {
                    if (content == .string) {
                        return try allocator.dupe(u8, content.string);
                    }
                }
            }
        }
    }

    return try allocator.dupe(u8, response_bytes);
}

pub fn main() !void {
    var gpa: std.heap.GeneralPurposeAllocator(.{}) = .{};
    defer _ = gpa.deinit();
    const allocator = gpa.allocator();

    const api_key = std.posix.getenv("OPENROUTER_API_KEY") orelse {
        std.debug.print("Error: OPENROUTER_API_KEY not set\n", .{});
        return;
    };

    const prompt = "How many r's are in the word 'strawberry'?";
    const response = try InvokeIntelligence(allocator, api_key, prompt);
    defer allocator.free(response);

    const stdout_file = std.fs.File.stdout();
    var stdout_buf: [4096]u8 = undefined;
    var fw = stdout_file.writer(&stdout_buf);
    const stdout = &fw.interface;

    stdout.print("{s}\n", .{response}) catch {};
    stdout.flush() catch {};
}
