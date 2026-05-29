using System.Text;
using System.Text.Json;

var apiKey = Environment.GetEnvironmentVariable("OPENROUTER_API_KEY");
if (string.IsNullOrEmpty(apiKey))
{
    Console.Error.WriteLine("OPENROUTER_API_KEY not set");
    return;
}

var messages = new object[]
{
    new { role = "system", content = "You are an assistant." },
    new { role = "user", content = "How many r's are in the word strawberry?" }
};

var request = new
{
    model = "nvidia/nemotron-3-super-120b-a12b:free",
    messages = messages,
    reasoning = new { enabled = false },
    stream = true
};

var json = JsonSerializer.Serialize(request);
var content = new StringContent(json, Encoding.UTF8, "application/json");

var httpClient = new HttpClient();
httpClient.DefaultRequestHeaders.Add("Authorization", $"Bearer {apiKey}");

try
{
    using var requestMsg = new HttpRequestMessage(HttpMethod.Post,
        "https://openrouter.ai/api/v1/chat/completions") { Content = content };
    using var response = await httpClient.SendAsync(requestMsg,
        HttpCompletionOption.ResponseHeadersRead);
    
    if (!response.IsSuccessStatusCode)
    {
        Console.Error.WriteLine($"HTTP {(int)response.StatusCode}: {await response.Content.ReadAsStringAsync()}");
        return;
    }

    using var stream = await response.Content.ReadAsStreamAsync();
    using var reader = new StreamReader(stream);
    
    while (!reader.EndOfStream)
    {
        var line = await reader.ReadLineAsync();
        if (string.IsNullOrWhiteSpace(line)) continue;
        if (!line.StartsWith("data: ")) continue;
        
        var data = line.Substring("data: ".Length).Trim();
        if (data == "[DONE]") break;
        
        using var doc = JsonDocument.Parse(data);
        if (doc.RootElement.TryGetProperty("choices", out var choices) &&
            choices.GetArrayLength() > 0 &&
            choices[0].TryGetProperty("delta", out var delta) &&
            delta.TryGetProperty("content", out var contentProp) &&
            contentProp.ValueKind == JsonValueKind.String)
        {
            Console.Write(contentProp.GetString());
        }
    }
    
    Console.WriteLine();
}
catch (Exception e)
{
    Console.Error.WriteLine($"Error: {e.Message}");
}
